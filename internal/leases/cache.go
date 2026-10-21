package leases

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rarebit-one/void-which-binds-go/grant"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// ErrNoCachedLease means no cached lease is even ABOUT this request — the cache
// is empty for it, or every cached lease names a different principal or resource.
//
// It is deliberately distinct from a lease that IS about the request but is
// refused — expired, not yet valid, capability denied — which surfaces the
// grant.Reason instead (via the wrapped grant error). A degraded peer must be
// able to tell "I hold no authority for this" from "I hold one and it lapsed",
// because only the second names `expired` to the caller (§54, #285).
var ErrNoCachedLease = errors.New("leases: no cached lease authorises this request")

// CacheStore is the CONSUMER half of the access-lease system (§53, §54, #285):
// the signed lease tokens a peer fetched from its siblings BEFORE an outage, and
// the decision of whether any of them authorises a request — made locally, by
// the peer's own clock, reaching nobody.
//
// [Store] is the issuer; this is what a surviving peer consults to answer a read
// with the controller gone. It caches TOKENS, which are opaque and
// self-authorising: the signature verifies against a sibling this peer pinned
// (ADR-0012), so a cached lease needs no row at the issuer and no network to
// honour — exactly the cross-site property [Store.Honour] relies on. The cache
// is a mirror of each sibling's active set; [CacheStore.Replace] overwrites what
// was held for a sibling, so a healthy re-fetch drops whatever the issuer let
// lapse and this peer never prunes by expiry on its own.
//
// What enters the cache from a sibling goes through [CacheStore.Refresh], which
// keeps only the device write leases that sibling signed for this site.
type CacheStore struct {
	writer   *sql.DB
	reader   *sql.DB
	events   *events.Log
	siblings SiblingKeys
	selfID   string
	selfKey  ed25519.PublicKey
}

// ErrUnpinnedSource is a refresh from a source this peer has not pinned as a
// sibling (or from itself). Nothing it served could verify, and treating that
// as "it serves nothing" would wipe what was cached from it, so the refresh is
// refused and the cache left as it was.
var ErrUnpinnedSource = errors.New("leases: the source is not a pinned sibling")

// CacheOptions configure a CacheStore.
type CacheOptions struct {
	// Writer is the single-writer pool (ADR-0003). Required.
	Writer *sql.DB
	// Reader serves authorise-time reads off the write path; nil uses Writer.
	Reader *sql.DB
	// Events records each change to the cache (lease.cached, invariant 7).
	// Required.
	Events *events.Log
	// Siblings supplies the pinned issuer keys a cached lease may verify against
	// — the same trust set [Store] honours by (ADR-0012 membership). Nil means
	// only self-issued leases verify, which is a single-peer deployment.
	Siblings SiblingKeys
	// SelfID / SelfKey are this peer's own identity, included in the trust set so
	// a lease this peer issued and cached is honourable too. Both or neither.
	SelfID  string
	SelfKey ed25519.PublicKey
}

// NewCache constructs a CacheStore.
func NewCache(opts CacheOptions) (*CacheStore, error) {
	if opts.Writer == nil {
		return nil, errors.New("leases: a writer database is required")
	}
	if opts.Events == nil {
		return nil, errors.New("leases: an event log is required (invariant 7)")
	}
	reader := opts.Reader
	if reader == nil {
		reader = opts.Writer
	}
	return &CacheStore{
		writer:   opts.Writer,
		reader:   reader,
		events:   opts.Events,
		siblings: opts.Siblings,
		selfID:   opts.SelfID,
		selfKey:  opts.SelfKey,
	}, nil
}

// CacheChange counts what one Replace or Refresh did to one source's cache.
type CacheChange struct {
	// Added is the tokens cached that were not held from this source before.
	Added int
	// Removed is the tokens held from this source that it no longer serves
	// (or that no longer verify, on a Refresh).
	Removed int
	// OtherSite is the served leases a Refresh skipped because they name
	// another site. A sibling serves every lease it issued to every member,
	// so these are expected wherever there are more than two sites.
	OtherSite int
	// Rejected is the served tokens a Refresh refused to cache that were
	// meant for this site or were unreadable: not signed by the source, not a
	// device write lease, or already expired on this peer's clock.
	Rejected int
}

// Changed reports whether the cache for the source is different afterwards.
func (c CacheChange) Changed() bool { return c.Added > 0 || c.Removed > 0 }

// Refresh mirrors what a sibling served (GET /peer/v1/leases) into the cache,
// keeping only what verifies: a token is cached only if it is signed by
// sourcePeer's pinned key (the rendered key membership holds), is a device
// write lease whose resource is THIS site, and has not expired on this peer's
// clock. A lease for another site is counted as OtherSite and everything else
// that fails as Rejected; neither is stored.
//
// It verifies here, before anything is stored, rather than leaving it all to
// Authorise, so the cache holds only leases that could lift a write here, and
// a sibling cannot fill this peer's table with leases it did not sign or that
// name another site. Authorise still verifies again at honour time, against the
// trust set as it is then.
//
// sourcePeer must be a pinned sibling, never this peer: anything else returns
// ErrUnpinnedSource and leaves the cache untouched. It changes the cache only
// through Replace, so a change is one transaction and one lease.cached event.
func (c *CacheStore) Refresh(ctx context.Context, sourcePeer string, tokens []string, now time.Time) (CacheChange, error) {
	if c.selfID == "" {
		return CacheChange{}, errors.New("leases: refreshing from a sibling needs this peer's own id (CacheOptions.SelfID)")
	}
	if sourcePeer == c.selfID || c.siblings == nil {
		return CacheChange{}, fmt.Errorf("%w: %s", ErrUnpinnedSource, sourcePeer)
	}
	keys, err := c.siblings.PeerKeys(ctx)
	if err != nil {
		return CacheChange{}, fmt.Errorf("leases: reading sibling keys: %w", err)
	}
	pub, ok := keys[sourcePeer]
	if !ok {
		return CacheChange{}, fmt.Errorf("%w: %s", ErrUnpinnedSource, sourcePeer)
	}
	trust := grant.Keys{sourcePeer: pub}
	site := SiteResource(c.selfID)

	kept := make([]string, 0, len(tokens))
	var otherSite, rejected int
	for _, token := range tokens {
		switch checkServedLease(token, trust, site, now) {
		case servedForThisSite:
			kept = append(kept, token)
		case servedForOtherSite:
			otherSite++
		default:
			rejected++
		}
	}
	change, err := c.Replace(ctx, sourcePeer, kept, now)
	change.OtherSite, change.Rejected = otherSite, rejected
	return change, err
}

type servedVerdict int

const (
	servedRejected servedVerdict = iota
	servedForOtherSite
	servedForThisSite
)

// checkServedLease reports whether a token a sibling served is a device write
// lease for site, signed by the one key in trust, and unexpired at now; or a
// lease naming another site, which is not this peer's to verify or keep.
//
// grant.Verify checks a token against a request, so the request is built from
// the token's own principal and resource. Those are read before the signature
// is checked, but only to say which lease to check for: Verify then proves the
// signature over the whole body, the issuer and the window, so a forged or
// altered field fails there.
func checkServedLease(token string, trust grant.Keys, site string, now time.Time) servedVerdict {
	body, _, ok := strings.Cut(token, ".")
	if !ok {
		return servedRejected
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return servedRejected
	}
	var claims struct {
		Principal string `json:"prn"`
		Resource  string `json:"res"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return servedRejected
	}
	if claims.Resource != site && strings.HasPrefix(claims.Resource, SiteResourcePrefix) {
		// A lease for another site can never lift a write here: only the site
		// it names honours it.
		return servedForOtherSite
	}
	if claims.Resource != site ||
		!strings.HasPrefix(claims.Principal, DevicePrincipalPrefix) || len(claims.Principal) == len(DevicePrincipalPrefix) {
		return servedRejected
	}
	if _, err := grant.Verify(token, trust, grant.Request{
		Principal: claims.Principal, Resource: claims.Resource, Capability: grant.CapabilityWrite,
	}, now); err != nil {
		return servedRejected
	}
	return servedForThisSite
}

// Replace makes the tokens cached FOR one source peer exactly tokens, in one
// transaction. Per source, because the cache mirrors each sibling's active set
// independently: re-fetching from A must drop what A let lapse without
// disturbing what was cached from B. Passing an empty slice is how a sibling
// that now holds no active leases is cleared.
//
// It writes only the difference, and records it as one lease.cached event
// (invariant 7). A set equal to what is held writes nothing and emits nothing:
// the same leases fetched again are not a transition. A token kept across
// fetches keeps the fetched_at of the fetch that first cached it.
//
// Replace stores what it is given; [CacheStore.Refresh] is the verifying path a
// sibling fetch takes.
func (c *CacheStore) Replace(ctx context.Context, sourcePeer string, tokens []string, now time.Time) (CacheChange, error) {
	want := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		want[t] = true
	}

	tx, err := c.writer.BeginTx(ctx, nil)
	if err != nil {
		return CacheChange{}, err
	}
	defer func() { _ = tx.Rollback() }()

	held, err := heldTokens(ctx, tx, sourcePeer)
	if err != nil {
		return CacheChange{}, err
	}
	var change CacheChange
	for token := range held {
		if want[token] {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM cached_leases WHERE source_peer = ? AND token = ?`, sourcePeer, token); err != nil {
			return CacheChange{}, fmt.Errorf("leases: dropping a cached lease from %s: %w", sourcePeer, err)
		}
		change.Removed++
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	for token := range want {
		if held[token] {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cached_leases (source_peer, token, fetched_at) VALUES (?, ?, ?)`,
			sourcePeer, token, stamp); err != nil {
			return CacheChange{}, fmt.Errorf("leases: caching a lease from %s: %w", sourcePeer, err)
		}
		change.Added++
	}
	if !change.Changed() {
		return change, nil // rolled back: nothing was written
	}
	ev, err := c.events.EmitTx(ctx, tx, events.TypeLeaseCached, "peer", sourcePeer,
		map[string]any{"source_peer": sourcePeer, "added": change.Added, "removed": change.Removed})
	if err != nil {
		return CacheChange{}, fmt.Errorf("leases: recording the cache change for %s: %w", sourcePeer, err)
	}
	if err := tx.Commit(); err != nil {
		return CacheChange{}, err
	}
	c.events.Publish(ev)
	return change, nil
}

// Cached returns the tokens held from one source, in no particular order.
func (c *CacheStore) Cached(ctx context.Context, sourcePeer string) ([]string, error) {
	held, err := heldTokens(ctx, c.reader, sourcePeer)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(held))
	for token := range held {
		out = append(out, token)
	}
	return out, nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func heldTokens(ctx context.Context, q queryer, sourcePeer string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT token FROM cached_leases WHERE source_peer = ?`, sourcePeer)
	if err != nil {
		return nil, fmt.Errorf("leases: reading the leases cached from %s: %w", sourcePeer, err)
	}
	defer func() { _ = rows.Close() }()
	held := map[string]bool{}
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return nil, err
		}
		held[token] = true
	}
	return held, rows.Err()
}

// Authorise reports whether any cached lease permits req at now, verified locally
// against the pinned trust set — no controller, no network (§53).
//
// On success it returns the honoured grant. On failure it returns the refusal
// that is ABOUT this request when one exists — a lease that named this principal
// and resource but was expired surfaces grant's expiry error (so the caller can
// say `expired`), a capability-denied likewise — and only when nothing even
// named this request returns ErrNoCachedLease. A lease for a different resource
// or principal is a different lease, not a refusal of this one.
func (c *CacheStore) Authorise(ctx context.Context, req grant.Request, now time.Time) (grant.Grant, error) {
	trust, err := c.trust(ctx)
	if err != nil {
		return grant.Grant{}, err
	}
	rows, err := c.reader.QueryContext(ctx, `SELECT token FROM cached_leases`)
	if err != nil {
		return grant.Grant{}, fmt.Errorf("leases: reading cached leases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var aboutRefusal error
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return grant.Grant{}, err
		}
		g, verr := grant.Verify(token, trust, req, now)
		if verr == nil {
			return g, nil
		}
		// A refusal of a lease that WAS about this request (expired, not yet
		// valid, capability denied) is more informative than "wrong resource",
		// which just means a different lease. Keep the first such refusal so the
		// caller can name it.
		if aboutRefusal == nil && aboutThisRequest(verr) {
			aboutRefusal = verr
		}
	}
	if err := rows.Err(); err != nil {
		return grant.Grant{}, err
	}
	if aboutRefusal != nil {
		return grant.Grant{}, aboutRefusal
	}
	return grant.Grant{}, ErrNoCachedLease
}

// aboutThisRequest reports whether a verify error refused a lease that named the
// request's principal and resource — i.e. a lease meant for this read that could
// not be honoured, versus a lease meant for something else.
func aboutThisRequest(err error) bool {
	switch grant.ReasonFor(err) {
	case grant.ReasonExpired, grant.ReasonNotYetValid, grant.ReasonCapabilityDenied:
		return true
	default:
		return false
	}
}

// trust is this peer's own key (when it has one) plus every pinned sibling's —
// the set a cached lease's signature is checked against, read fresh each call so
// enrolling or revoking a peer takes effect at once, the property [Store]'s
// honour-time trust has.
func (c *CacheStore) trust(ctx context.Context) (grant.Keys, error) {
	trust := grant.Keys{}
	if c.selfID != "" && c.selfKey != nil {
		trust[c.selfID] = c.selfKey
	}
	if c.siblings != nil {
		keys, err := c.siblings.PeerKeys(ctx)
		if err != nil {
			return nil, fmt.Errorf("leases: reading sibling keys: %w", err)
		}
		for id, pub := range keys {
			if _, ok := trust[id]; !ok { // self already present; a sibling never overrides it
				trust[id] = pub
			}
		}
	}
	return trust, nil
}
