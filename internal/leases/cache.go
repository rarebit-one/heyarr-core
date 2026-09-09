package leases

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/grant"
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
type CacheStore struct {
	writer   *sql.DB
	reader   *sql.DB
	siblings SiblingKeys
	selfID   string
	selfKey  ed25519.PublicKey
}

// CacheOptions configure a CacheStore.
type CacheOptions struct {
	// Writer is the single-writer pool (ADR-0003). Required.
	Writer *sql.DB
	// Reader serves authorise-time reads off the write path; nil uses Writer.
	Reader *sql.DB
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
	reader := opts.Reader
	if reader == nil {
		reader = opts.Writer
	}
	return &CacheStore{
		writer:   opts.Writer,
		reader:   reader,
		siblings: opts.Siblings,
		selfID:   opts.SelfID,
		selfKey:  opts.SelfKey,
	}, nil
}

// Replace overwrites the tokens cached FOR one source peer with a freshly fetched
// set, in one transaction. Per source, because the cache mirrors each sibling's
// active set independently: re-fetching from A must drop what A let lapse without
// disturbing what was cached from B. Passing an empty slice is how a sibling that
// now holds no active leases is cleared.
func (c *CacheStore) Replace(ctx context.Context, sourcePeer string, tokens []string, now time.Time) error {
	tx, err := c.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM cached_leases WHERE source_peer = ?`, sourcePeer); err != nil {
		return fmt.Errorf("leases: clearing cached leases for %s: %w", sourcePeer, err)
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, token := range tokens {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO cached_leases (source_peer, token, fetched_at) VALUES (?, ?, ?)`,
			sourcePeer, token, stamp); err != nil {
			return fmt.Errorf("leases: caching a lease from %s: %w", sourcePeer, err)
		}
	}
	return tx.Commit()
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
