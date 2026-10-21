package controller

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/grant"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// The request path's trust set is peer.lease_issuers ∩ membership, this node
// excluded, read on every call (ADR-0048 amendment, #285). Driven through the
// controller's own wiring (deviceWriteLeases) over a real membership store and
// lease cache: a lease from an opted-in member lifts; the same lease from a
// member that is not opted in, from this node, or from an opted-in sibling
// after it is removed from membership, does not, and says unknown_issuer.
func TestTheRequestPathHonoursOnlyOptedInMembers(t *testing.T) {
	at := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

	type world struct {
		members          *membership.Store
		selfID, aID, bID string // membership peer ids
		selfKey, a, b    ed25519.PrivateKey
		device           string
	}
	setup := func(t *testing.T) (world, func(optIn []string) *leases.CacheStore) {
		t.Helper()
		db := testdb.Migrated(t)
		eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: &movableClock{t: at}})
		if err != nil {
			t.Fatal(err)
		}
		members, err := membership.New(membership.Options{DB: db, Events: eventLog, Logger: discard()})
		if err != nil {
			t.Fatal(err)
		}
		var w world
		w.members = members
		for _, k := range []*ed25519.PrivateKey{&w.selfKey, &w.a, &w.b} {
			if _, *k, err = ed25519.GenerateKey(nil); err != nil {
				t.Fatal(err)
			}
		}
		ids := []*string{&w.selfID, &w.aID, &w.bID}
		for i, r := range []membership.Registration{
			{Name: "peer-s", Site: "site-s", PublicKey: w.selfKey.Public().(ed25519.PublicKey), IsSelf: true},
			{Name: "peer-a", Site: "site-a", PublicKey: w.a.Public().(ed25519.PublicKey), Endpoint: "https://peer-a.invalid:7780"},
			{Name: "peer-b", Site: "site-b", PublicKey: w.b.Public().(ed25519.PublicKey), Endpoint: "https://peer-b.invalid:7780"},
		} {
			res, err := members.Register(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			*ids[i] = res.Member.PeerID
		}
		devPub, _, _ := ed25519.GenerateKey(nil)
		w.device = identity.FormatPublicKey(devPub)

		build := func(optIn []string) *leases.CacheStore {
			cfg := config.Defaults()
			cfg.Peer.LeaseIssuers = optIn
			c := &Controller{cfg: cfg, log: discard()}
			got, err := c.deviceWriteLeases(context.Background(), db, eventLog, members, w.selfKey.Public().(ed25519.PublicKey))
			if err != nil {
				t.Fatal(err)
			}
			cache, ok := got.(*leases.CacheStore)
			if !ok {
				t.Fatalf("deviceWriteLeases = %T, want the lease cache", got)
			}
			return cache
		}
		return w, build
	}

	// lease is the lease `by`'s issuing beat would mint for the device here.
	lease := func(t *testing.T, w world, by ed25519.PrivateKey) string {
		t.Helper()
		tok, err := grant.Grant{
			Issuer:       identity.FormatPublicKey(by.Public().(ed25519.PublicKey)),
			Principal:    leases.DevicePrincipal(w.device),
			Resource:     leases.SiteResource(identity.FormatPublicKey(w.selfKey.Public().(ed25519.PublicKey))),
			Capabilities: []grant.Capability{grant.CapabilityWrite},
			IssuedAt:     at, ExpiresAt: at.Add(leases.DeviceWriteTTL),
		}.Sign(by)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	tests := []struct {
		name   string
		optIn  func(w world) []string
		signer func(w world) ed25519.PrivateKey
		remove func(w world) string // a peer id removed from membership before the check
		lifted bool
	}{
		{
			name:   "an opted-in member lifts",
			optIn:  func(w world) []string { return []string{w.aID} },
			signer: func(w world) ed25519.PrivateKey { return w.a },
			lifted: true,
		},
		{
			name:   "a member that is not opted in does not",
			optIn:  func(w world) []string { return []string{w.aID} },
			signer: func(w world) ed25519.PrivateKey { return w.b },
		},
		{
			name:   "an empty opt-in honours no member",
			optIn:  func(world) []string { return nil },
			signer: func(w world) ed25519.PrivateKey { return w.a },
		},
		{
			name:   "an opted-in sibling removed from membership stops counting at once",
			optIn:  func(w world) []string { return []string{w.aID} },
			signer: func(w world) ed25519.PrivateKey { return w.a },
			remove: func(w world) string { return w.aID },
		},
		{
			name:   "naming this node honours nothing",
			optIn:  func(w world) []string { return []string{w.selfID} },
			signer: func(w world) ed25519.PrivateKey { return w.selfKey },
		},
		{
			name:   "a peer's name is not its id",
			optIn:  func(world) []string { return []string{"peer-a"} },
			signer: func(w world) ed25519.PrivateKey { return w.a },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, build := setup(t)
			cache := build(tt.optIn(w))
			if _, err := cache.Replace(context.Background(), "source", []string{lease(t, w, tt.signer(w))}, at); err != nil {
				t.Fatal(err)
			}
			if tt.remove != nil {
				if _, err := w.members.Remove(context.Background(), tt.remove(w)); err != nil {
					t.Fatal(err)
				}
			}
			issuer, err := cache.LiftDeviceWrite(context.Background(), w.device, at)
			if tt.lifted {
				want := identity.FormatPublicKey(tt.signer(w).Public().(ed25519.PublicKey))
				if err != nil || issuer != want {
					t.Fatalf("LiftDeviceWrite = (%q, %v), want a lift by %q", issuer, err, want)
				}
				return
			}
			if r, _ := leases.RefusalFor(err); r != leases.RefusalUnknownIssuer {
				t.Fatalf("LiftDeviceWrite = (%q, %v), want refusal %q", issuer, err, leases.RefusalUnknownIssuer)
			}
		})
	}
}

// With no key of its own this node is no site a lease could name, so no lease
// check is wired at all.
func TestNoSelfKeyWiresNoLeaseCheck(t *testing.T) {
	db := testdb.Migrated(t)
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	members, err := membership.New(membership.Options{DB: db, Events: eventLog, Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{cfg: config.Defaults(), log: discard()}
	got, err := c.deviceWriteLeases(context.Background(), db, eventLog, members, nil)
	if err != nil || got != nil {
		t.Fatalf("deviceWriteLeases with no self key = (%v, %v), want (nil, nil)", got, err)
	}
}
