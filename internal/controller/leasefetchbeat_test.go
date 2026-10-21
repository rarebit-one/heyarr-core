package controller

import (
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/grant"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/leasesync"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// scriptedFetcher answers every fetch with what the test last set: tokens, or
// an error standing in for a sibling that cannot be reached.
type scriptedFetcher struct {
	mu     sync.Mutex
	tokens []string
	err    error
	calls  int
	asked  []string
}

func (f *scriptedFetcher) FetchLeases(_ context.Context, t leasesync.Target) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.asked = append(f.asked, t.Peer.Name)
	return slices.Clone(f.tokens), f.err
}

func (f *scriptedFetcher) answer(tokens []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens, f.err = tokens, err
}

func (f *scriptedFetcher) seen() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, slices.Clone(f.asked)
}

// The sibling lease fetch beat is wired to the controller's real membership
// and lease cache, runs at startup and on the ticker it is handed, and fetches
// from the one sibling in membership: a fetch caches the lease that sibling
// issued for this site, an unreachable sibling leaves it cached, and a fetch
// where the sibling serves none clears it. The mechanism has a caller (#362).
func TestLeaseFetchBeatCachesFromTheControllersSiblings(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	db := testdb.Migrated(t)
	clock := &movableClock{t: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	members, err := membership.New(membership.Options{DB: db, Events: eventLog, Logger: discard()})
	if err != nil {
		t.Fatal(err)
	}
	selfPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	siblingPub, siblingKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []membership.Registration{
		{Name: "peer-b", Site: "site-b", Mode: "full", PublicKey: selfPub, IsSelf: true, Endpoint: "https://peer-b.invalid:7780"},
		{Name: "peer-a", Site: "site-a", Mode: "full", PublicKey: siblingPub, Endpoint: "https://peer-a.invalid:7780"},
	} {
		if _, err := members.Register(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	// What the sibling serves: one device write lease for this site, signed by
	// the sibling, from a lease store of its own.
	siblingDB := testdb.Migrated(t)
	siblingEvents, err := events.New(events.Options{Writer: siblingDB.Writer(), Reader: siblingDB.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	siblingStore, err := leases.New(leases.Options{
		Writer: siblingDB.Writer(), Reader: siblingDB.Reader(), Events: siblingEvents, Signer: siblingKey, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := siblingStore.Issue(ctx, leases.DevicePrincipal("ed25519:0d0d"),
		leases.SiteResource(identity.FormatPublicKey(selfPub)), []grant.Capability{grant.CapabilityWrite}, leases.DeviceWriteTTL)
	if err != nil {
		t.Fatal(err)
	}

	fetcher := &scriptedFetcher{}
	fetcher.answer([]string{lease.Token}, nil)
	ticker := newFakeTicker()
	if err := startSiblingLeaseFetch(ctx, db, eventLog, selfPub, members, fetcher, clock, discard(), ticker.newTicker); err != nil {
		t.Fatal(err)
	}
	if ticker.made != 1 || ticker.interval != leaseFetchInterval {
		t.Fatalf("made %d tickers at %v, want one at %v", ticker.made, ticker.interval, leaseFetchInterval)
	}

	// A second handle over the same table, to read what the beat cached.
	reader, err := leases.NewCache(leases.CacheOptions{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	siblingID := identity.FormatPublicKey(siblingPub)
	cached := func() []string {
		t.Helper()
		got, err := reader.Cached(ctx, siblingID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	cachedEvents := func() int {
		t.Helper()
		evs, err := eventLog.Since(ctx, 0, []string{events.TypeLeaseCached}, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	// pass lets the beat take a tick and waits for that pass to finish: a
	// second send is taken only once the first one's pass has returned.
	pass := func() {
		t.Helper()
		ticker.fire(t)
		ticker.fire(t)
	}

	steps := []struct {
		name   string
		tokens []string
		err    error
		cached []string
		events int
	}{
		{"startup and a tick cache the sibling's lease", []string{lease.Token}, nil, []string{lease.Token}, 1},
		{"an unreachable sibling leaves it cached", nil, errors.New("dial: connection refused"), []string{lease.Token}, 1},
		{"a sibling that now serves none clears it", []string{}, nil, []string{}, 2},
	}
	for i, s := range steps {
		if i > 0 {
			fetcher.answer(s.tokens, s.err)
		}
		pass()
		if got := cached(); !slices.Equal(got, s.cached) {
			t.Fatalf("%s: cached %d tokens, want %d", s.name, len(got), len(s.cached))
		}
		if got := cachedEvents(); got != s.events {
			t.Fatalf("%s: %d lease.cached events, want %d", s.name, got, s.events)
		}
	}
	calls, asked := fetcher.seen()
	// One startup pass plus at least one per tick: the startup pass is what
	// makes the first step's count possible before the ticks.
	if calls < 1+len(steps) {
		t.Errorf("the beat fetched %d times, want at least %d (startup plus a tick per step)", calls, 1+len(steps))
	}
	for _, name := range asked {
		if name != "peer-a" {
			t.Fatalf("the beat fetched from %q; it must fetch from the sibling only, never from itself", name)
		}
	}

	cancel()
	ticker.awaitStop(t)
}
