package leases_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/grant"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// refreshHarness is site B's cache, pinning siblings A and C, with B's event
// log so a test can read the lease.cached events a refresh records.
type refreshHarness struct {
	cache    *leases.CacheStore
	events   *events.Log
	selfID   string
	keyA     ed25519.PrivateKey
	keyC     ed25519.PrivateKey
	idA, idC string
}

func newRefreshHarness(t *testing.T) *refreshHarness {
	t.Helper()
	db := testdb.Migrated(t)
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: &fixedClock{t: now}})
	if err != nil {
		t.Fatal(err)
	}
	selfPub, _, _ := ed25519.GenerateKey(nil)
	_, keyA, _ := ed25519.GenerateKey(nil)
	_, keyC, _ := ed25519.GenerateKey(nil)
	h := &refreshHarness{events: log, keyA: keyA, keyC: keyC, idA: idOf(keyA), idC: idOf(keyC)}
	h.selfID = identity.FormatPublicKey(selfPub)
	h.cache, err = leases.NewCache(leases.CacheOptions{
		Writer: db.Writer(), Reader: db.Reader(), Events: log,
		Siblings: fakeSiblings{
			h.idA: keyA.Public().(ed25519.PublicKey),
			h.idC: keyC.Public().(ed25519.PublicKey),
		},
		SelfID: h.selfID, SelfKey: selfPub,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *refreshHarness) cached(t *testing.T, source string) []string {
	t.Helper()
	got, err := h.cache.Cached(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	return got
}

func (h *refreshHarness) cachedEvents(t *testing.T) []events.Event {
	t.Helper()
	evs, err := h.events.Since(context.Background(), 0, []string{events.TypeLeaseCached}, 100)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// Refresh keeps exactly the served tokens that are device write leases for
// this site, signed by the sibling that served them and unexpired here.
func TestRefreshCachesOnlyWhatVerifies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const device = "ed25519:0d0d"
	write := []grant.Capability{grant.CapabilityWrite}

	cases := []struct {
		name      string
		token     func(h *refreshHarness) string
		kept      bool
		otherSite bool
	}{
		{"a device write lease for this site, signed by the source", func(h *refreshHarness) string {
			return issue(t, h.keyA, leases.DevicePrincipal(device), leases.SiteResource(h.selfID), write, leases.DeviceWriteTTL)
		}, true, false},
		{"a lease for another site", func(h *refreshHarness) string {
			return issue(t, h.keyA, leases.DevicePrincipal(device), leases.SiteResource(h.idC), write, leases.DeviceWriteTTL)
		}, false, true},
		{"a read lease for this site", func(h *refreshHarness) string {
			return issue(t, h.keyA, leases.DevicePrincipal(device), leases.SiteResource(h.selfID),
				[]grant.Capability{grant.CapabilityRead}, leases.DeviceWriteTTL)
		}, false, false},
		{"a lease naming a user, not a device", func(h *refreshHarness) string {
			return issue(t, h.keyA, "user-a", leases.SiteResource(h.selfID), write, leases.DeviceWriteTTL)
		}, false, false},
		{"a lease another pinned sibling signed, relayed by the source", func(h *refreshHarness) string {
			return issue(t, h.keyC, leases.DevicePrincipal(device), leases.SiteResource(h.selfID), write, leases.DeviceWriteTTL)
		}, false, false},
		{"a lease already expired on this peer's clock", func(h *refreshHarness) string {
			return issue(t, h.keyA, leases.DevicePrincipal(device), leases.SiteResource(h.selfID), write, time.Minute)
		}, false, false},
		{"not a token at all", func(*refreshHarness) string { return "not-a-token" }, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newRefreshHarness(t)
			token := tc.token(h)
			change, err := h.cache.Refresh(ctx, h.idA, []string{token}, now)
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			got := h.cached(t, h.idA)
			if tc.kept {
				if len(got) != 1 || got[0] != token || change.Added != 1 || change.Rejected != 0 {
					t.Fatalf("cached %d tokens, change %+v; want the one token added", len(got), change)
				}
				return
			}
			want := leases.CacheChange{Rejected: 1}
			if tc.otherSite {
				want = leases.CacheChange{OtherSite: 1}
			}
			if len(got) != 0 || change != want {
				t.Fatalf("cached %d tokens, change %+v; want nothing cached and %+v", len(got), change, want)
			}
		})
	}
}

// A refresh from a source this peer has not pinned, or from itself, changes
// nothing: it is refused before the cache is touched, so what was cached from a
// real sibling under that name is never wiped by it.
func TestRefreshFromAnUnpinnedSourceLeavesTheCacheAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newRefreshHarness(t)
	lease := issue(t, h.keyA, leases.DevicePrincipal("ed25519:0d0d"), leases.SiteResource(h.selfID),
		[]grant.Capability{grant.CapabilityWrite}, leases.DeviceWriteTTL)
	if _, err := h.cache.Refresh(ctx, h.idA, []string{lease}, now); err != nil {
		t.Fatal(err)
	}
	_, stranger, _ := ed25519.GenerateKey(nil)

	for _, source := range []string{idOf(stranger), h.selfID} {
		if _, err := h.cache.Refresh(ctx, source, nil, now); !errors.Is(err, leases.ErrUnpinnedSource) {
			t.Errorf("Refresh from %s = %v, want ErrUnpinnedSource", source, err)
		}
	}
	if got := h.cached(t, h.idA); len(got) != 1 {
		t.Fatalf("A's cache after refused refreshes holds %d, want 1", len(got))
	}
}

// Replace records one lease.cached per change, with counts, and nothing for a
// fetch that brought the same set back.
func TestReplaceEmitsLeaseCachedOnlyOnChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newRefreshHarness(t)

	steps := []struct {
		name             string
		tokens           []string
		events           int
		added, removed   int
		wantPayloadCount bool
	}{
		{"first fetch caches two", []string{"t1", "t2"}, 1, 2, 0, true},
		{"the same set again is a no-op", []string{"t2", "t1"}, 1, 0, 0, false},
		{"one dropped, one new", []string{"t2", "t3"}, 2, 1, 1, true},
		{"the sibling now serves none", nil, 3, 0, 2, true},
		{"none again is a no-op", []string{}, 3, 0, 0, false},
	}
	for _, s := range steps {
		change, err := h.cache.Replace(ctx, h.idA, s.tokens, now)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if change.Added != s.added || change.Removed != s.removed {
			t.Errorf("%s: change %+v, want added %d removed %d", s.name, change, s.added, s.removed)
		}
		evs := h.cachedEvents(t)
		if len(evs) != s.events {
			t.Fatalf("%s: %d lease.cached events, want %d", s.name, len(evs), s.events)
		}
		if !s.wantPayloadCount {
			continue
		}
		last := evs[len(evs)-1]
		if last.SubjectID != h.idA {
			t.Errorf("%s: event subject %q, want the sibling %q", s.name, last.SubjectID, h.idA)
		}
		var p struct {
			Source  string `json:"source_peer"`
			Added   int    `json:"added"`
			Removed int    `json:"removed"`
		}
		if err := json.Unmarshal(last.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Source != h.idA || p.Added != s.added || p.Removed != s.removed {
			t.Errorf("%s: payload %+v, want {%s %d %d}", s.name, p, h.idA, s.added, s.removed)
		}
	}
}

func issue(t *testing.T, signer ed25519.PrivateKey, principal, resource string, caps []grant.Capability, ttl time.Duration) string {
	t.Helper()
	l, err := peerStore(t, signer, nil).Issue(context.Background(), principal, resource, caps, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return l.Token
}
