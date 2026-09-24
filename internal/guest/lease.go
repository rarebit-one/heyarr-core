package guest

import (
	"context"
	"fmt"
	"time"

	"github.com/rarebit-one/voidbind-go/grant"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/leases"
)

// DefaultTTL is the life of a guest access lease (ADR-0094). It is deliberately
// short and well under grant.MaxTTL (24h): a guest is admitted by its source
// address, not by a credential it holds, so the lease is a bounded window rather
// than a durable grant — an address that leaves the trusted net stops being a
// guest within the hour rather than for a day. It re-mints transparently on the
// next request from an address still inside the boundary.
const DefaultTTL = time.Hour

// LeaseStore is the slice of the M7 lease store a guest minter needs: issue a
// signed lease, and list the active ones so an existing lease for a source is
// reused rather than a fresh row written per request. *leases.Store satisfies
// it. It is narrow on purpose — the guest tier needs to mint and look up, not
// revoke or honour, which the store does elsewhere.
type LeaseStore interface {
	Issue(ctx context.Context, principal, resource string, caps []grant.Capability, ttl time.Duration) (leases.Lease, error)
	ActiveLeases(ctx context.Context, now time.Time) ([]leases.Lease, error)
}

// Minter admits a credential-less caller from a trusted source as a guest,
// backed by an access lease (ADR-0094). It is NOT a parallel auth path: the
// lease it mints is an ordinary internal/leases lease — the store's first
// non-device principal — so a guest session is listable, revocable and audited
// exactly as a cross-site lease is.
type Minter struct {
	store LeaseStore
	ttl   time.Duration
}

// NewMinter builds a guest minter over a lease store. A zero ttl uses DefaultTTL.
func NewMinter(store LeaseStore, ttl time.Duration) *Minter {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Minter{store: store, ttl: ttl}
}

// resourceFor is the lease resource a guest from source is scoped to: the shared
// library, keyed by the caller's source address so mint and lookup are per
// address (ADR-0094 design call 3). The key is what makes an active lease
// reusable for a source rather than a new row per request, and what a later
// revocation would target.
func resourceFor(source string) string {
	return "guest-library:" + source
}

// GuestLease admits source as a guest and returns the identity it acts as,
// carrying the lease's capabilities. It reuses an unexpired lease for the same
// source and mints one only when none is live, so a browsing guest is one row,
// not one per request. now is the caller's clock (ADR-0017), used to decide
// which leases are still active; the store signs the minted lease on its own
// injected clock, which a test wires to the same instant.
func (m *Minter) GuestLease(ctx context.Context, source string, now time.Time) (auth.Identity, error) {
	resource := resourceFor(source)

	active, err := m.store.ActiveLeases(ctx, now)
	if err != nil {
		return auth.Identity{}, fmt.Errorf("guest: looking up an active lease: %w", err)
	}
	for _, l := range active {
		if l.Principal == Principal && l.Resource == resource {
			return identityWith(l.Capabilities), nil
		}
	}

	l, err := m.store.Issue(ctx, Principal, resource, Capabilities(), m.ttl)
	if err != nil {
		return auth.Identity{}, fmt.Errorf("guest: minting a lease: %w", err)
	}
	return identityWith(l.Capabilities), nil
}
