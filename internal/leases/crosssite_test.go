package leases_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/grant"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// fakeSiblings is a static SiblingKeys, the peer set B has pinned.
type fakeSiblings map[string]ed25519.PublicKey

func (f fakeSiblings) PeerKeys(context.Context) (map[string]ed25519.PublicKey, error) {
	return f, nil
}

// peerStore builds a store for one peer with a given signer and pinned siblings.
func peerStore(t *testing.T, signer ed25519.PrivateKey, siblings leases.SiblingKeys) *leases.Store {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	clock := &fixedClock{t: now}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	store, err := leases.New(leases.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: log, Signer: signer, Siblings: siblings, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// The whole point of ADR-0048: a lease minted at peer A is honoured at peer B,
// because B has A pinned — and B reaches NOBODY to check it (the Honour call
// makes no network request; that is the cross-site property made literal).
func TestALeaseFromSiteAIsHonouredAtSiteB(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Peer A issues a lease.
	_, keyA, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	peerA := peerStore(t, keyA, nil)
	lease, err := peerA.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	if err != nil {
		t.Fatalf("A issue: %v", err)
	}

	// Peer B has A pinned as a sibling, and its own distinct identity.
	_, keyB, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	idA := identity.FormatPublicKey(keyA.Public().(ed25519.PublicKey))
	peerB := peerStore(t, keyB, fakeSiblings{idA: keyA.Public().(ed25519.PublicKey)})

	// B honours A's lease — no row for it in B, no reach to A.
	g, err := peerB.Honour(ctx, lease.Token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now)
	if err != nil {
		t.Fatalf("B should honour A's lease across sites, got %v", err)
	}
	if g.Issuer != idA {
		t.Fatalf("honoured grant issuer = %q, want A's %q", g.Issuer, idA)
	}
}

// A peer that has NOT pinned the issuer refuses the lease — enrol before trust,
// the same gate that stops a stranger's lease opening anything.
func TestAnUnpinnedIssuersLeaseIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, keyA, _ := ed25519.GenerateKey(nil)
	peerA := peerStore(t, keyA, nil)
	lease, err := peerA.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Peer B does NOT pin A.
	_, keyB, _ := ed25519.GenerateKey(nil)
	peerB := peerStore(t, keyB, fakeSiblings{})
	if _, err := peerB.Honour(ctx, lease.Token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now); !errors.Is(err, grant.ErrUnknownIssuer) {
		t.Fatalf("an unpinned issuer's lease should be refused, got %v", err)
	}
}

// The cross-site revocation asymmetry, end to end. A revokes a lease; A refuses
// it immediately (it holds the row). B, holding A's lease across a partition,
// has no row and keeps honouring it until it EXPIRES — the stated consequence
// the 24h cap bounds (ADR-0048), not a bug.
func TestRevocationAtTheIssuerDoesNotReachASibling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, keyA, _ := ed25519.GenerateKey(nil)
	peerA := peerStore(t, keyA, nil)
	lease, err := peerA.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	idA := identity.FormatPublicKey(keyA.Public().(ed25519.PublicKey))
	_, keyB, _ := ed25519.GenerateKey(nil)
	peerB := peerStore(t, keyB, fakeSiblings{idA: keyA.Public().(ed25519.PublicKey)})

	// A revokes.
	if _, err := peerA.Revoke(ctx, lease.ID); err != nil {
		t.Fatal(err)
	}
	// A refuses at once.
	if _, err := peerA.Honour(ctx, lease.Token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now); !errors.Is(err, leases.ErrLeaseRevoked) {
		t.Fatalf("A should refuse its revoked lease, got %v", err)
	}
	// B still honours it — it cannot know, and honours until expiry.
	if _, err := peerB.Honour(ctx, lease.Token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now); err != nil {
		t.Fatalf("B has no way to know A revoked; it should still honour, got %v", err)
	}
	// But B refuses it once it expires, on B's own clock.
	if _, err := peerB.Honour(ctx, lease.Token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now.Add(2*time.Hour)); !errors.Is(err, grant.ErrExpired) {
		t.Fatalf("B should refuse the expired lease by its own clock, got %v", err)
	}
}

// A revoked SIBLING (removed from B's pinned set) stops verifying at once — the
// per-request trust read, the peer-membership property applied to leases.
func TestARemovedSiblingStopsVerifying(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, keyA, _ := ed25519.GenerateKey(nil)
	peerA := peerStore(t, keyA, nil)
	lease, err := peerA.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	idA := identity.FormatPublicKey(keyA.Public().(ed25519.PublicKey))

	// A mutable sibling set B can drop A from.
	siblings := &mutableSiblings{keys: map[string]ed25519.PublicKey{idA: keyA.Public().(ed25519.PublicKey)}}
	_, keyB, _ := ed25519.GenerateKey(nil)
	peerB := peerStore(t, keyB, siblings)

	if _, err := peerB.Honour(ctx, lease.Token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now); err != nil {
		t.Fatalf("precondition: B honours A's lease, got %v", err)
	}
	// B revokes A's membership.
	siblings.drop(idA)
	if _, err := peerB.Honour(ctx, lease.Token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now); !errors.Is(err, grant.ErrUnknownIssuer) {
		t.Fatalf("after removing A, B should refuse A's leases at once, got %v", err)
	}
}

type mutableSiblings struct{ keys map[string]ed25519.PublicKey }

func (m *mutableSiblings) PeerKeys(context.Context) (map[string]ed25519.PublicKey, error) {
	out := make(map[string]ed25519.PublicKey, len(m.keys))
	for k, v := range m.keys {
		out[k] = v
	}
	return out, nil
}

func (m *mutableSiblings) drop(id string) { delete(m.keys, id) }
