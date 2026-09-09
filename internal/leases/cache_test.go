package leases_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/grant"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// newCache builds a consumer CacheStore over a fresh db with the given pinned
// siblings — the trust set a cached lease's signature verifies against.
func newCache(t *testing.T, siblings leases.SiblingKeys) *leases.CacheStore {
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
	c, err := leases.NewCache(leases.CacheOptions{Writer: db.Writer(), Reader: db.Reader(), Siblings: siblings})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func idOf(k ed25519.PrivateKey) string {
	return identity.FormatPublicKey(k.Public().(ed25519.PublicKey))
}

// A cached lease permits a read with nobody to ask — and its ABSENCE refuses,
// which is the half that makes the first half mean anything (#285).
func TestCachedLeasePermitsAReadAndItsAbsenceRefuses(t *testing.T) {
	ctx := context.Background()
	_, keyA, _ := ed25519.GenerateKey(nil)
	peerA := peerStore(t, keyA, nil)
	lease, err := peerA.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	idA := idOf(keyA)
	cache := newCache(t, fakeSiblings{idA: keyA.Public().(ed25519.PublicKey)})
	req := grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}

	if _, err := cache.Authorise(ctx, req, now); !errors.Is(err, leases.ErrNoCachedLease) {
		t.Fatalf("an empty cache = %v, want ErrNoCachedLease", err)
	}
	if err := cache.Replace(ctx, idA, []string{lease.Token}, now); err != nil {
		t.Fatal(err)
	}
	g, err := cache.Authorise(ctx, req, now)
	if err != nil {
		t.Fatalf("a cached lease should permit the read, got %v", err)
	}
	if g.Issuer != idA {
		t.Errorf("honoured grant issuer = %q, want A's %q", g.Issuer, idA)
	}
}

// An expired lease is refused by the cache ALONE, by its own clock (injected, not
// a sleep — ADR-0017), and the refusal NAMES `expired`.
func TestCacheRefusesAnExpiredLeaseNamingExpired(t *testing.T) {
	ctx := context.Background()
	_, keyA, _ := ed25519.GenerateKey(nil)
	peerA := peerStore(t, keyA, nil)
	lease, _ := peerA.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	idA := idOf(keyA)
	cache := newCache(t, fakeSiblings{idA: keyA.Public().(ed25519.PublicKey)})
	if err := cache.Replace(ctx, idA, []string{lease.Token}, now); err != nil {
		t.Fatal(err)
	}

	req := grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}
	// Two hours on — past the one-hour lease — the cache refuses it itself.
	_, err := cache.Authorise(ctx, req, now.Add(2*time.Hour))
	if grant.ReasonFor(err) != grant.ReasonExpired {
		t.Fatalf("expired lease refused with reason %q, want %q", grant.ReasonFor(err), grant.ReasonExpired)
	}
}

// A lease for a different resource or principal is a DIFFERENT lease — it does
// not open this one, and Authorise says so as "no cached lease", not as a
// refusal of a lease that was about this request.
func TestACachedLeaseForAnotherResourceOrPrincipalDoesNotOpenThisOne(t *testing.T) {
	ctx := context.Background()
	_, keyA, _ := ed25519.GenerateKey(nil)
	peerA := peerStore(t, keyA, nil)
	lease, _ := peerA.Issue(ctx, "user-a", "asset-OTHER", []grant.Capability{grant.CapabilityRead}, time.Hour)
	idA := idOf(keyA)
	cache := newCache(t, fakeSiblings{idA: keyA.Public().(ed25519.PublicKey)})
	if err := cache.Replace(ctx, idA, []string{lease.Token}, now); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.Authorise(ctx, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now); !errors.Is(err, leases.ErrNoCachedLease) {
		t.Errorf("a lease for another resource = %v, want ErrNoCachedLease", err)
	}
	if _, err := cache.Authorise(ctx, grant.Request{Principal: "user-b", Resource: "asset-OTHER", Capability: grant.CapabilityRead}, now); !errors.Is(err, leases.ErrNoCachedLease) {
		t.Errorf("a lease for another principal = %v, want ErrNoCachedLease", err)
	}
}

// Capabilities are honoured: a read lease does not permit a write, and the
// refusal names that (a §53 "No" operation, ADR-0040).
func TestAReadLeaseDoesNotPermitAWrite(t *testing.T) {
	ctx := context.Background()
	_, keyA, _ := ed25519.GenerateKey(nil)
	peerA := peerStore(t, keyA, nil)
	lease, _ := peerA.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	idA := idOf(keyA)
	cache := newCache(t, fakeSiblings{idA: keyA.Public().(ed25519.PublicKey)})
	if err := cache.Replace(ctx, idA, []string{lease.Token}, now); err != nil {
		t.Fatal(err)
	}

	_, err := cache.Authorise(ctx, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityWrite}, now)
	if grant.ReasonFor(err) != grant.ReasonCapabilityDenied {
		t.Fatalf("a read lease used for a write = %q, want capability_denied", grant.ReasonFor(err))
	}
}

// A lease signed by a key this peer has NOT pinned is not honoured — the same
// ADR-0012 gate the issuer's cross-site honour has, cheap insurance that the
// cache did not open a second door.
func TestALeaseFromAnUnpinnedIssuerIsNotHonoured(t *testing.T) {
	ctx := context.Background()
	_, stranger, _ := ed25519.GenerateKey(nil)
	peerX := peerStore(t, stranger, nil)
	lease, _ := peerX.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)

	cache := newCache(t, fakeSiblings{}) // nobody pinned
	if err := cache.Replace(ctx, "stranger", []string{lease.Token}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Authorise(ctx, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now); !errors.Is(err, leases.ErrNoCachedLease) {
		t.Fatalf("an unpinned issuer's lease = %v, want ErrNoCachedLease", err)
	}
}

// Replace mirrors a sibling's active set: a re-fetch that no longer includes a
// token drops it, so a lease the issuer let lapse stops authorising without this
// peer pruning by expiry on its own.
func TestReplaceMirrorsTheSiblingsActiveSet(t *testing.T) {
	ctx := context.Background()
	_, keyA, _ := ed25519.GenerateKey(nil)
	peerA := peerStore(t, keyA, nil)
	one, _ := peerA.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	idA := idOf(keyA)
	cache := newCache(t, fakeSiblings{idA: keyA.Public().(ed25519.PublicKey)})
	req := grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}

	if err := cache.Replace(ctx, idA, []string{one.Token}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Authorise(ctx, req, now); err != nil {
		t.Fatalf("cached lease should authorise: %v", err)
	}
	// A refresh where the sibling now advertises nothing clears it.
	if err := cache.Replace(ctx, idA, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Authorise(ctx, req, now); !errors.Is(err, leases.ErrNoCachedLease) {
		t.Fatalf("after a refresh that dropped it = %v, want ErrNoCachedLease", err)
	}
}
