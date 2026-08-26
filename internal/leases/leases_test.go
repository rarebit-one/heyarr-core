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

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

var now = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) (*leases.Store, ed25519.PrivateKey) {
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
	_, signer, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := leases.New(leases.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: log, Signer: signer, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, signer
}

func TestIssueAndHonour(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	ctx := context.Background()

	lease, err := store.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if lease.Token == "" || lease.ExpiresAt != now.Add(time.Hour) {
		t.Fatalf("issued lease is wrong: %+v", lease)
	}
	g, err := store.Honour(ctx, lease.Token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now)
	if err != nil {
		t.Fatalf("a valid lease was refused: %v", err)
	}
	if g.Principal != "user-a" || g.Resource != "asset-1" {
		t.Fatalf("honoured grant wrong: %+v", g)
	}
}

// Honour passes the four bindings through to grant.Verify, so each still
// constrains — a lease is not a bearer token (ADR-0048).
func TestHonourConstrainsEachBinding(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	ctx := context.Background()
	lease, err := store.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		req  grant.Request
		want error
	}{
		{"wrong principal", grant.Request{Principal: "user-b", Resource: "asset-1", Capability: grant.CapabilityRead}, grant.ErrPrincipalMismatch},
		{"wrong resource", grant.Request{Principal: "user-a", Resource: "asset-2", Capability: grant.CapabilityRead}, grant.ErrResourceMismatch},
		{"read lease, write asked", grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityWrite}, grant.ErrCapabilityDenied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.Honour(ctx, lease.Token, tc.req, now); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// Expiry is refused by the honouring clock (grant.Verify), proven by advancing
// the injected clock past the lease's life.
func TestHonourRefusesExpiredLease(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	ctx := context.Background()
	lease, err := store.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Honour(ctx, lease.Token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now.Add(2*time.Hour)); !errors.Is(err, grant.ErrExpired) {
		t.Fatalf("want grant.ErrExpired, got %v", err)
	}
}

// The 24h cap is enforced at issue: an over-long lease cannot be minted (the
// revocation window cannot be widened by issuing).
func TestIssueEnforcesTTLCap(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	ctx := context.Background()
	if _, err := store.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, grant.MaxTTL+time.Minute); !errors.Is(err, grant.ErrTTLTooLong) {
		t.Fatalf("want grant.ErrTTLTooLong, got %v", err)
	}
	// Exactly the cap is allowed.
	if _, err := store.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, grant.MaxTTL); err != nil {
		t.Fatalf("a lease of exactly MaxTTL should issue, got %v", err)
	}
}

// The issuer refuses a lease it has revoked. This is the reachable-issuer half
// of the asymmetric model.
func TestIssuerRefusesRevokedLease(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	ctx := context.Background()
	lease, err := store.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(ctx, lease.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := store.Honour(ctx, lease.Token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now); !errors.Is(err, leases.ErrLeaseRevoked) {
		t.Fatalf("the issuer should refuse its revoked lease, got %v", err)
	}
}

// The cross-site half: a lease honoured on its SIGNATURE alone, with no row in
// the honouring store — the degraded-read property (§53). A store that does not
// hold the lease's row has no revocation say over it and honours it until it
// expires. Simulated by signing a valid token with the same issuer key but
// never storing it (as a sibling peer's cached lease would arrive).
func TestHonourWithoutARowUsesSignatureAlone(t *testing.T) {
	t.Parallel()
	store, signer := newStore(t)
	ctx := context.Background()

	// A lease this store never issued (no row), but signed by the trusted issuer.
	issuerID := identity.FormatPublicKey(signer.Public().(ed25519.PublicKey))
	token, err := grant.Grant{
		Issuer:       issuerID,
		Principal:    "user-a",
		Resource:     "asset-1",
		Capabilities: []grant.Capability{grant.CapabilityRead},
		IssuedAt:     now,
		ExpiresAt:    now.Add(time.Hour),
	}.Sign(signer)
	if err != nil {
		t.Fatal(err)
	}
	// Honoured — verified by signature, and no row means no revocation to check.
	if _, err := store.Honour(ctx, token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now); err != nil {
		t.Fatalf("a signature-valid lease with no local row should be honoured, got %v", err)
	}
	// And it still expires by the local clock.
	if _, err := store.Honour(ctx, token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now.Add(2*time.Hour)); !errors.Is(err, grant.ErrExpired) {
		t.Fatalf("even a rowless lease expires by the clock, got %v", err)
	}
}

// A lease signed by a key this store does not trust is refused unknown_issuer —
// the enrol-before-trust gate. (#285 widens trust to sibling peers; here only
// this peer's own key is trusted.)
func TestHonourRefusesUntrustedIssuer(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	ctx := context.Background()
	_, stranger, _ := ed25519.GenerateKey(nil)
	strangerID := identity.FormatPublicKey(stranger.Public().(ed25519.PublicKey))
	token, err := grant.Grant{
		Issuer: strangerID, Principal: "user-a", Resource: "asset-1",
		Capabilities: []grant.Capability{grant.CapabilityRead}, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}.Sign(stranger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Honour(ctx, token, grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}, now); !errors.Is(err, grant.ErrUnknownIssuer) {
		t.Fatalf("want grant.ErrUnknownIssuer, got %v", err)
	}
}

// A store built without a signer cannot issue, but can still honour and revoke —
// a read-only replica's shape.
func TestIssueRefusedWithoutSigner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := leases.New(leases.Options{Writer: db.Writer(), Reader: db.Reader(), Events: log})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour); !errors.Is(err, leases.ErrNoSigner) {
		t.Fatalf("want ErrNoSigner, got %v", err)
	}
}
