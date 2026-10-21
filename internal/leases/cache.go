package leases

import (
	"bytes"
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
// is empty for it, or every cached lease names a different principal or resource,
// or is signed by an issuer this peer does not trust for it.
//
// It is deliberately distinct from a lease that IS about the request but is
// refused — expired, not yet valid, capability denied — which surfaces the
// grant.Reason instead (via the wrapped grant error). A degraded peer must be
// able to tell "I hold no authority for this" from "I hold one and it lapsed",
// because only the second names `expired` to the caller (§54, #285).
//
// Every refusal Authorise returns is also a *RefusalError, so the reason can be
// asserted by name ([RefusalFor]): a lease naming another device is
// ErrNoCachedLease AND principal_mismatch, not just the first.
var ErrNoCachedLease = errors.New("leases: no cached lease authorises this request")

// A Refusal names why no cached lease authorised a request. It is a closed set,
// safe as a metric label: each value is one of grant's reasons, folded where
// an operator would act the same way, plus RefusalNoLease.
//
// The values share words (expired / not_yet_valid, principal_ / resource_
// mismatch), so a caller compares them exactly, never by substring.
type Refusal string

// The refusals Authorise and LiftDeviceWrite report.
const (
	// RefusalNoLease is a cache holding nothing that could be checked against
	// this request: no cached lease at all.
	RefusalNoLease Refusal = "no_lease"
	// RefusalExpired is a lease for exactly this request whose life, less
	// grant.SkewMargin, is over on this peer's clock.
	RefusalExpired Refusal = "expired"
	// RefusalNotYetValid is a lease for exactly this request issued in this
	// peer's future, beyond the skew margin.
	RefusalNotYetValid Refusal = "not_yet_valid"
	// RefusalCapabilityDenied is a lease for this principal and resource that
	// does not carry the capability asked for (a read lease asked for write).
	RefusalCapabilityDenied Refusal = "capability_denied"
	// RefusalResourceMismatch is a lease for this principal at another resource
	// (for a device write lease: another site).
	RefusalResourceMismatch Refusal = "resource_mismatch"
	// RefusalPrincipalMismatch is a trusted lease naming another principal (for
	// a device write lease: another device key).
	RefusalPrincipalMismatch Refusal = "principal_mismatch"
	// RefusalUnknownIssuer is a lease signed by a key outside the trust set:
	// not pinned, or, on the request path, pinned but not opted into.
	RefusalUnknownIssuer Refusal = "unknown_issuer"
	// RefusalInvalid is a cached token that does not verify at all: malformed,
	// of another token type, or with a signature its trusted issuer's key
	// rejects. Refresh never stores one, so it means a corrupted cache.
	RefusalInvalid Refusal = "invalid"
)

// RefusalError is a request a cached lease did not authorise, with its reason
// by name. It wraps the underlying error, so errors.Is still finds
// ErrNoCachedLease and grant's sentinels (grant.ErrExpired, ...) through it.
// An error that is NOT a RefusalError (a database or trust-set read failing)
// is an operational failure, not a refusal.
type RefusalError struct {
	Reason Refusal
	err    error
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("leases: refused (%s): %v", e.Reason, e.err)
}

// Unwrap exposes the grant error or ErrNoCachedLease underneath.
func (e *RefusalError) Unwrap() error { return e.err }

// LeaseRefusal returns the reason as a plain string, so a caller can name the
// refusal without importing this package (internal/api/http does).
func (e *RefusalError) LeaseRefusal() string { return string(e.Reason) }

// RefusalFor returns the named reason of a refusal Authorise or
// LiftDeviceWrite returned, and false for nil or an operational error.
func RefusalFor(err error) (Refusal, bool) {
	var r *RefusalError
	if errors.As(err, &r) {
		return r.Reason, true
	}
	return "", false
}

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
	honoured SiblingKeys
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
	// Honoured supplies the siblings whose write leases LiftDeviceWrite honours:
	// the peers this node opted into (peer.lease_issuers) that are still in
	// membership, read on every call so a sibling removed from either stops
	// counting at once. This peer is never in it, whatever the source says.
	// Nil honours none, which is the default (ADR-0048 amendment, #285).
	Honoured SiblingKeys
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
		honoured: opts.Honoured,
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
// On success it returns the honoured grant. On failure it returns a
// *RefusalError naming the refusal of the cached lease that came CLOSEST to
// authorising req ([refusalRank]), so the caller can say `expired` for a lease
// that lapsed rather than "no lease". A refusal that is about this request
// (expired, not yet valid, capability denied) unwraps to grant's error alone; one
// that is about a different lease (another principal or resource, an issuer this
// peer does not trust, a token that does not verify) also unwraps to
// ErrNoCachedLease, as it always did. An empty cache is RefusalNoLease.
func (c *CacheStore) Authorise(ctx context.Context, req grant.Request, now time.Time) (grant.Grant, error) {
	trust, err := c.trust(ctx)
	if err != nil {
		return grant.Grant{}, err
	}
	return c.authorise(ctx, trust, req, now)
}

// LiftDeviceWrite reports whether a cached lease lifts deviceKey to write at
// this site: an unexpired `{device:<deviceKey>, site:<this peer>, write}` lease
// signed by a sibling in Honoured (ADR-0048 amendment, #285). It returns the
// honoured lease's issuer, the sibling's rendered public key.
//
// deviceKey must be the key a Device credential's possession proof has just
// proved. Nothing a client presents is read here: the lease is looked up, never
// accepted, so a lease is never a bearer credential.
//
// The trust set is Honoured alone, with this peer removed. Unlike Authorise it
// does not include every pinned sibling: a sibling that is pinned but not opted
// into is refused `unknown_issuer`, the same as a key nobody pinned. With
// Honoured nil or empty every cached lease is refused, so the answer is never a
// lift. Refusals are *RefusalError; anything else is an operational failure,
// and the caller must treat both as "no lift".
func (c *CacheStore) LiftDeviceWrite(ctx context.Context, deviceKey string, now time.Time) (string, error) {
	if c.selfID == "" {
		return "", errors.New("leases: lifting a device to write needs this peer's own id (CacheOptions.SelfID)")
	}
	if deviceKey == "" {
		return "", errors.New("leases: lifting a device to write needs the device key its credential proved")
	}
	trust, err := c.honouredTrust(ctx)
	if err != nil {
		return "", err
	}
	g, err := c.authorise(ctx, trust, grant.Request{
		Principal:  DevicePrincipal(deviceKey),
		Resource:   SiteResource(c.selfID),
		Capability: grant.CapabilityWrite,
	}, now)
	if err != nil {
		return "", err
	}
	return g.Issuer, nil
}

// honouredTrust is the trust set the request path verifies a write lease
// against: the opted-in siblings Honoured reports, never this peer, and never
// a key of the wrong size. It is read on every call, like trust.
func (c *CacheStore) honouredTrust(ctx context.Context) (grant.Keys, error) {
	trust := grant.Keys{}
	if c.honoured == nil {
		return trust, nil
	}
	keys, err := c.honoured.PeerKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("leases: reading the honoured lease issuers: %w", err)
	}
	for id, pub := range keys {
		if id == c.selfID || len(pub) != ed25519.PublicKeySize || bytes.Equal(pub, c.selfKey) {
			continue
		}
		trust[id] = pub
	}
	return trust, nil
}

// authorise checks every cached lease against req under trust. Any lease that
// verifies authorises; otherwise the refusal of the lease that got furthest
// through grant.Verify is returned, named.
func (c *CacheStore) authorise(ctx context.Context, trust grant.Keys, req grant.Request, now time.Time) (grant.Grant, error) {
	rows, err := c.reader.QueryContext(ctx, `SELECT token FROM cached_leases`)
	if err != nil {
		return grant.Grant{}, fmt.Errorf("leases: reading cached leases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		best     error
		bestRank = -1
	)
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return grant.Grant{}, err
		}
		g, verr := grant.Verify(token, trust, req, now)
		if verr == nil {
			return g, nil
		}
		// Keep the first refusal of the highest rank, so the reason reported is
		// the one about the lease nearest to authorising this request.
		if r := refusalRank(verr); r > bestRank {
			best, bestRank = verr, r
		}
	}
	if err := rows.Err(); err != nil {
		return grant.Grant{}, err
	}
	if best == nil {
		return grant.Grant{}, &RefusalError{Reason: RefusalNoLease, err: ErrNoCachedLease}
	}
	reason := refusalOf(best)
	if !aboutThisRequest(reason) {
		best = fmt.Errorf("%w: %w", ErrNoCachedLease, best)
	}
	return grant.Grant{}, &RefusalError{Reason: reason, err: best}
}

// refusalOf names a grant.Verify error.
func refusalOf(err error) Refusal {
	switch grant.ReasonFor(err) {
	case grant.ReasonExpired:
		return RefusalExpired
	case grant.ReasonNotYetValid:
		return RefusalNotYetValid
	case grant.ReasonCapabilityDenied:
		return RefusalCapabilityDenied
	case grant.ReasonResourceMismatch:
		return RefusalResourceMismatch
	case grant.ReasonPrincipalMismatch:
		return RefusalPrincipalMismatch
	case grant.ReasonUnknownIssuer:
		return RefusalUnknownIssuer
	default:
		return RefusalInvalid
	}
}

// refusalRank orders refusals by how far the lease got through grant.Verify,
// which checks the issuer is trusted, then the signature, the principal, the
// resource, the capability and last the clock. A lease that failed only on the
// clock matched everything else, so it says the most about this request; one
// from an untrusted issuer says the least. Ranking only picks which refusal is
// REPORTED: it never turns a refusal into a lift.
func refusalRank(err error) int {
	switch refusalOf(err) {
	case RefusalExpired, RefusalNotYetValid:
		return 6
	case RefusalCapabilityDenied:
		return 5
	case RefusalResourceMismatch:
		return 4
	case RefusalPrincipalMismatch:
		return 3
	case RefusalUnknownIssuer:
		return 1
	default:
		// A token that does not parse, or whose trusted issuer's key rejects its
		// signature (that one passed the issuer check, so it ranks above
		// unknown_issuer).
		if grant.ReasonFor(err) == grant.ReasonBadSignature {
			return 2
		}
		return 0
	}
}

// aboutThisRequest reports whether a refusal is of a lease that named the
// request's principal and resource, i.e. a lease meant for this request that
// could not be honoured, versus a lease meant for something else.
func aboutThisRequest(r Refusal) bool {
	switch r {
	case RefusalExpired, RefusalNotYetValid, RefusalCapabilityDenied:
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
