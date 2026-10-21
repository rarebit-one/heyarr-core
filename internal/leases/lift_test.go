package leases_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/grant"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// liftWorld is site S's view for LiftDeviceWrite: sibling A is pinned and opted
// into, sibling B is pinned but not opted into, X is pinned by nobody, and D and
// E are two device keys.
type liftWorld struct {
	selfKey, keyA, keyB, keyX ed25519.PrivateKey
	selfID, idA, idB          string
	devD, devE                string
}

func newLiftWorld(t *testing.T) liftWorld {
	t.Helper()
	var w liftWorld
	for _, k := range []*ed25519.PrivateKey{&w.selfKey, &w.keyA, &w.keyB, &w.keyX} {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		*k = priv
	}
	w.selfID, w.idA, w.idB = idOf(w.selfKey), idOf(w.keyA), idOf(w.keyB)
	d, _, _ := ed25519.GenerateKey(nil)
	e, _, _ := ed25519.GenerateKey(nil)
	w.devD, w.devE = identity.FormatPublicKey(d), identity.FormatPublicKey(e)
	return w
}

func pub(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

// signLease mints a lease token directly, so a test can put any binding in it
// (a lease the issuing beat would never mint, standing in for one a forged or
// stale cache could hold).
func signLease(t *testing.T, by ed25519.PrivateKey, principal, resource string, caps []grant.Capability, issued time.Time) string {
	t.Helper()
	tok, err := grant.Grant{
		Issuer: idOf(by), Principal: principal, Resource: resource, Capabilities: caps,
		IssuedAt: issued, ExpiresAt: issued.Add(leases.DeviceWriteTTL),
	}.Sign(by)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// newLiftCache is S's cache holding tokens, trusting A and B as pinned
// siblings and honouring whatever honoured says.
func newLiftCache(t *testing.T, w liftWorld, honoured leases.SiblingKeys, tokens []string) *leases.CacheStore {
	t.Helper()
	db := testdb.Migrated(t)
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: &fixedClock{t: now}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := leases.NewCache(leases.CacheOptions{
		Writer: db.Writer(), Reader: db.Reader(), Events: log,
		Siblings: fakeSiblings{w.idA: pub(w.keyA), w.idB: pub(w.keyB)},
		Honoured: honoured,
		SelfID:   w.selfID, SelfKey: pub(w.selfKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Replace stores without verifying, so the cache can hold what Refresh
	// would have refused: Authorise must not depend on Refresh having run.
	if _, err := c.Replace(context.Background(), "source", tokens, now); err != nil {
		t.Fatal(err)
	}
	return c
}

type failingKeys struct{}

func (failingKeys) PeerKeys(context.Context) (map[string]ed25519.PublicKey, error) {
	return nil, errors.New("membership is unreadable")
}

// LiftDeviceWrite lifts only on an unexpired {device:<D>, site:<self>, write}
// lease signed by an opted-in sibling, and names every other answer. Each
// refusal is asserted by its enum value AND by the grant sentinel it wraps,
// never by a substring (ADR-0048: the reasons share words).
func TestLiftDeviceWriteNamesEachRefusal(t *testing.T) {
	w := newLiftWorld(t)
	site := leases.SiteResource(w.selfID)
	dev := leases.DevicePrincipal(w.devD)
	write := []grant.Capability{grant.CapabilityWrite}
	optedInA := fakeSiblings{w.idA: pub(w.keyA)}
	// grant.Verify refuses from expiry minus the skew margin, on this peer's
	// clock: the boundary is the second the honoured window ends.
	refuseAt := now.Add(leases.DeviceWriteTTL - grant.SkewMargin)

	tests := []struct {
		name     string
		honoured leases.SiblingKeys
		tokens   func() []string
		at       time.Time
		lifted   bool
		want     leases.Refusal
		sentinel error // what errors.Is must also find
		noLease  bool  // whether the refusal is also ErrNoCachedLease
	}{
		{
			name: "a lease from an opted-in sibling lifts", honoured: optedInA,
			tokens: func() []string { return []string{signLease(t, w.keyA, dev, site, write, now)} },
			at:     now, lifted: true,
		},
		{
			name: "one second inside the skew margin still lifts", honoured: optedInA,
			tokens: func() []string { return []string{signLease(t, w.keyA, dev, site, write, now)} },
			at:     refuseAt.Add(-time.Second), lifted: true,
		},
		{
			name: "an empty cache is no lease", honoured: optedInA,
			tokens: func() []string { return nil },
			at:     now, want: leases.RefusalNoLease, sentinel: leases.ErrNoCachedLease, noLease: true,
		},
		{
			name: "at expiry minus the skew margin it is expired", honoured: optedInA,
			tokens: func() []string { return []string{signLease(t, w.keyA, dev, site, write, now)} },
			at:     refuseAt, want: leases.RefusalExpired, sentinel: grant.ErrExpired,
		},
		{
			name: "a lease for device E does not lift device D", honoured: optedInA,
			tokens: func() []string {
				return []string{signLease(t, w.keyA, leases.DevicePrincipal(w.devE), site, write, now)}
			},
			at: now, want: leases.RefusalPrincipalMismatch, sentinel: grant.ErrPrincipalMismatch, noLease: true,
		},
		{
			name: "a lease for another site does not lift here", honoured: optedInA,
			tokens: func() []string {
				return []string{signLease(t, w.keyA, dev, leases.SiteResource(w.idB), write, now)}
			},
			at: now, want: leases.RefusalResourceMismatch, sentinel: grant.ErrResourceMismatch, noLease: true,
		},
		{
			name: "a lease without write does not lift", honoured: optedInA,
			tokens: func() []string {
				return []string{signLease(t, w.keyA, dev, site, []grant.Capability{grant.CapabilityRead}, now)}
			},
			at: now, want: leases.RefusalCapabilityDenied, sentinel: grant.ErrCapabilityDenied,
		},
		{
			name: "a pinned sibling not opted into is an unknown issuer", honoured: optedInA,
			tokens: func() []string { return []string{signLease(t, w.keyB, dev, site, write, now)} },
			at:     now, want: leases.RefusalUnknownIssuer, sentinel: grant.ErrUnknownIssuer, noLease: true,
		},
		{
			name: "a key nobody pinned is an unknown issuer", honoured: optedInA,
			tokens: func() []string { return []string{signLease(t, w.keyX, dev, site, write, now)} },
			at:     now, want: leases.RefusalUnknownIssuer, sentinel: grant.ErrUnknownIssuer, noLease: true,
		},
		{
			name: "no honoured set honours none, even a valid lease", honoured: nil,
			tokens: func() []string { return []string{signLease(t, w.keyA, dev, site, write, now)} },
			at:     now, want: leases.RefusalUnknownIssuer, sentinel: grant.ErrUnknownIssuer, noLease: true,
		},
		{
			name: "an empty honoured set honours none", honoured: fakeSiblings{},
			tokens: func() []string { return []string{signLease(t, w.keyA, dev, site, write, now)} },
			at:     now, want: leases.RefusalUnknownIssuer, sentinel: grant.ErrUnknownIssuer, noLease: true,
		},
		{
			name:     "this peer is never an issuer, even when the honoured set names it",
			honoured: fakeSiblings{w.selfID: pub(w.selfKey)},
			tokens:   func() []string { return []string{signLease(t, w.selfKey, dev, site, write, now)} },
			at:       now, want: leases.RefusalUnknownIssuer, sentinel: grant.ErrUnknownIssuer, noLease: true,
		},
		{
			name: "a token that does not verify is invalid", honoured: optedInA,
			tokens: func() []string { return []string{"not-a-lease"} },
			at:     now, want: leases.RefusalInvalid, sentinel: grant.ErrMalformed, noLease: true,
		},
		{
			name: "the refusal nearest this request is the one named", honoured: optedInA,
			tokens: func() []string {
				return []string{
					signLease(t, w.keyB, dev, site, write, now),                            // unknown_issuer
					signLease(t, w.keyA, leases.DevicePrincipal(w.devE), site, write, now), // principal_mismatch
					signLease(t, w.keyA, dev, site, write, now.Add(-time.Hour)),            // expired at `at`
				}
			},
			at: refuseAt.Add(-30 * time.Minute), want: leases.RefusalExpired, sentinel: grant.ErrExpired,
		},
		{
			name: "any lease that verifies lifts, whatever else is cached", honoured: optedInA,
			tokens: func() []string {
				return []string{
					signLease(t, w.keyB, dev, site, write, now),
					signLease(t, w.keyA, dev, site, write, now),
				}
			},
			at: now, lifted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := newLiftCache(t, w, tt.honoured, tt.tokens())
			issuer, err := cache.LiftDeviceWrite(context.Background(), w.devD, tt.at)
			if tt.lifted {
				if err != nil {
					t.Fatalf("want a lift, got %v", err)
				}
				if issuer != w.idA {
					t.Fatalf("issuer = %q, want A's %q", issuer, w.idA)
				}
				return
			}
			if issuer != "" {
				t.Fatalf("a refusal returned issuer %q", issuer)
			}
			got, ok := leases.RefusalFor(err)
			if !ok {
				t.Fatalf("err = %v, want a named refusal", err)
			}
			if got != tt.want {
				t.Fatalf("refusal = %q, want %q (err %v)", got, tt.want, err)
			}
			if !errors.Is(err, tt.sentinel) {
				t.Errorf("err = %v, want it to wrap %v", err, tt.sentinel)
			}
			if errors.Is(err, leases.ErrNoCachedLease) != tt.noLease {
				t.Errorf("errors.Is(err, ErrNoCachedLease) = %v, want %v", !tt.noLease, tt.noLease)
			}
		})
	}
}

// An unreadable honoured set or a missing self id is an operational failure,
// not a refusal, and never a lift: the caller keeps the device on its floor.
func TestLiftDeviceWriteFailuresAreNotRefusals(t *testing.T) {
	w := newLiftWorld(t)
	lease := signLease(t, w.keyA, leases.DevicePrincipal(w.devD), leases.SiteResource(w.selfID),
		[]grant.Capability{grant.CapabilityWrite}, now)

	cache := newLiftCache(t, w, failingKeys{}, []string{lease})
	issuer, err := cache.LiftDeviceWrite(context.Background(), w.devD, now)
	if err == nil || issuer != "" {
		t.Fatalf("an unreadable honoured set = (%q, %v), want an error and no issuer", issuer, err)
	}
	if r, ok := leases.RefusalFor(err); ok {
		t.Fatalf("an unreadable honoured set was named %q; it is a failure, not a refusal", r)
	}

	cache = newLiftCache(t, w, fakeSiblings{w.idA: pub(w.keyA)}, []string{lease})
	if issuer, err := cache.LiftDeviceWrite(context.Background(), "", now); err == nil || issuer != "" {
		t.Fatalf("no device key = (%q, %v), want an error and no issuer", issuer, err)
	}

	db := testdb.Migrated(t)
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	noSelf, err := leases.NewCache(leases.CacheOptions{
		Writer: db.Writer(), Events: log, Honoured: fakeSiblings{w.idA: pub(w.keyA)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if issuer, err := noSelf.LiftDeviceWrite(context.Background(), w.devD, now); err == nil || issuer != "" {
		t.Fatalf("no self id = (%q, %v), want an error and no issuer", issuer, err)
	}
}

// Authorise, the existing caller, now names the refusals it used to fold into
// ErrNoCachedLease, and still matches ErrNoCachedLease for them.
func TestAuthoriseNamesALeaseForAnotherPrincipal(t *testing.T) {
	w := newLiftWorld(t)
	site := leases.SiteResource(w.selfID)
	lease := signLease(t, w.keyA, leases.DevicePrincipal(w.devE), site, []grant.Capability{grant.CapabilityWrite}, now)
	cache := newLiftCache(t, w, nil, []string{lease})

	_, err := cache.Authorise(context.Background(), grant.Request{
		Principal: leases.DevicePrincipal(w.devD), Resource: site, Capability: grant.CapabilityWrite,
	}, now)
	if r, _ := leases.RefusalFor(err); r != leases.RefusalPrincipalMismatch {
		t.Fatalf("refusal = %q, want %q", r, leases.RefusalPrincipalMismatch)
	}
	if !errors.Is(err, leases.ErrNoCachedLease) || !errors.Is(err, grant.ErrPrincipalMismatch) {
		t.Fatalf("err = %v, want both ErrNoCachedLease and grant.ErrPrincipalMismatch", err)
	}
}
