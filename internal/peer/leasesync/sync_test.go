package leasesync_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/grant"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/leasesync"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// These tests run two real sites: A issues device write leases and serves them
// on its real peer surface over mTLS, and B fetches them with the real pinned
// client into its real cache. Both read one clock the test moves (ADR-0017).

var start = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

type movableClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *movableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *movableClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type keySet map[string]ed25519.PublicKey

func (k keySet) PeerKeys(context.Context) (map[string]ed25519.PublicKey, error) { return k, nil }

type writeDevices struct {
	mu   sync.Mutex
	keys []string
}

func (w *writeDevices) WriteDeviceKeys(context.Context) ([]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.keys), nil
}

func (w *writeDevices) set(keys ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.keys = keys
}

// members is the trust root A's listener consults: B, by key.
type members map[string]mtls.Peer

func (m members) Lookup(_ context.Context, pub []byte) (mtls.Peer, error) {
	p, ok := m[string(pub)]
	if !ok {
		return mtls.Peer{}, mtls.ErrNotAMember
	}
	return p, nil
}

// targets is a Siblings a test can point somewhere else between passes.
type targets struct {
	mu sync.Mutex
	ts []leasesync.Target
}

func (s *targets) Siblings(context.Context) ([]leasesync.Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ts), nil
}

func (s *targets) set(ts ...leasesync.Target) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ts = ts
}

type site struct {
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	id       string // rendered key
	peer     mtls.Peer
	material *mtls.Material
}

func newSite(t *testing.T, peerID string) site {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	material, err := mtls.NewMaterial(mtls.MaterialOptions{
		PrivateKey: priv, PeerID: peerID, Lifetime: time.Hour, RenewBefore: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return site{
		pub: pub, priv: priv, id: identity.FormatPublicKey(pub), material: material,
		peer: mtls.Peer{PeerID: peerID, Name: peerID, PublicKey: pub},
	}
}

// pair is site A (issuer, serving) and site B (fetching, caching).
type pair struct {
	clock   *movableClock
	a, b    site
	devices *writeDevices
	issuerA *leases.DeviceWriteIssuer
	storeA  *leases.Store
	target  leasesync.Target
	sibs    *targets
	cacheB  *leases.CacheStore
	eventsB *events.Log
	syncer  *leasesync.Syncer
}

func newPair(t *testing.T) *pair {
	t.Helper()
	p := &pair{clock: &movableClock{t: start}, devices: &writeDevices{}, sibs: &targets{}}
	p.a, p.b = newSite(t, "peer-a"), newSite(t, "peer-b")

	dbA := testdb.Migrated(t)
	eventsA, err := events.New(events.Options{Writer: dbA.Writer(), Reader: dbA.Reader(), Clock: p.clock})
	if err != nil {
		t.Fatal(err)
	}
	p.storeA, err = leases.New(leases.Options{
		Writer: dbA.Writer(), Reader: dbA.Reader(), Events: eventsA, Signer: p.a.priv, Clock: p.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.issuerA, err = leases.NewDeviceWriteIssuer(p.storeA, p.devices, keySet{p.b.id: p.b.pub})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := peerapi.New(peerapi.Options{
		Addr: "127.0.0.1:0", Material: p.a.material,
		Members:    members{string(p.b.pub): p.b.peer},
		SelfPeerID: p.a.peer.PeerID, Leases: p.storeA,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	p.target = leasesync.Target{Peer: p.a.peer, Endpoint: "https://" + srv.Addr()}
	p.sibs.set(p.target)

	dbB := testdb.Migrated(t)
	p.eventsB, err = events.New(events.Options{Writer: dbB.Writer(), Reader: dbB.Reader(), Clock: p.clock})
	if err != nil {
		t.Fatal(err)
	}
	p.cacheB, err = leases.NewCache(leases.CacheOptions{
		Writer: dbB.Writer(), Reader: dbB.Reader(), Events: p.eventsB,
		Siblings: keySet{p.a.id: p.a.pub}, SelfID: p.b.id, SelfKey: p.b.pub,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.syncer = leasesync.NewSyncer(p.cacheB, leasesync.NewClient(p.b.material, nil), p.sibs, p.clock, nil)
	return p
}

// reconcileA runs A's issuer beat once, at the clock's now.
func (p *pair) reconcileA(t *testing.T) {
	t.Helper()
	if _, err := p.issuerA.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (p *pair) servedByA(t *testing.T) []string {
	t.Helper()
	tokens, err := p.storeA.ActiveLeaseTokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(tokens)
	return tokens
}

func (p *pair) cachedAtB(t *testing.T) []string {
	t.Helper()
	tokens, err := p.cacheB.Cached(context.Background(), p.a.id)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(tokens)
	return tokens
}

func (p *pair) sync(t *testing.T) leasesync.Result {
	t.Helper()
	res, err := p.syncer.SyncAll(context.Background())
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	return res
}

type cachedPayload struct {
	Source  string `json:"source_peer"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

func (p *pair) cachedEvents(t *testing.T) []cachedPayload {
	t.Helper()
	evs, err := p.eventsB.Since(context.Background(), 0, []string{events.TypeLeaseCached}, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]cachedPayload, len(evs))
	for i, ev := range evs {
		if err := json.Unmarshal(ev.Payload, &out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// unreachable is an endpoint nothing listens on: a port that was free a moment
// ago, so a dial is refused at once rather than timing out.
func unreachable(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "https://" + addr
}

const (
	deviceD = "ed25519:0d0d"
	deviceE = "ed25519:0e0e"
)

// One fetch mirrors A's active device write set into B's cache and records one
// lease.cached; a revocation at A is gone from B after the next fetch; a fetch
// that brings back the same set changes nothing and records nothing.
func TestSyncMirrorsTheSiblingsActiveSetAndRecordsOnlyChanges(t *testing.T) {
	t.Parallel()
	p := newPair(t)
	p.devices.set(deviceD, deviceE)
	p.reconcileA(t)
	served := p.servedByA(t)
	if len(served) != 2 {
		t.Fatalf("A serves %d leases, want 2 (one per device for site B)", len(served))
	}

	steps := []struct {
		name    string
		before  func()
		want    func() []string
		events  []cachedPayload
		changed int
	}{
		{
			name:    "the first fetch caches A's set",
			before:  func() {},
			want:    func() []string { return served },
			events:  []cachedPayload{{p.a.id, 2, 0}},
			changed: 1,
		},
		{
			name:    "the same set again is a no-op",
			before:  func() {},
			want:    func() []string { return served },
			events:  []cachedPayload{{p.a.id, 2, 0}},
			changed: 0,
		},
		{
			name: "a lease A revoked is gone after the next fetch",
			before: func() {
				p.devices.set(deviceD) // E's grant deleted at A
				p.reconcileA(t)
			},
			want:    func() []string { return p.servedByA(t) },
			events:  []cachedPayload{{p.a.id, 2, 0}, {p.a.id, 0, 1}},
			changed: 1,
		},
	}
	for _, s := range steps {
		s.before()
		res := p.sync(t)
		if res.Fetched != 1 || res.Deferred != 0 || res.Changed != s.changed {
			t.Errorf("%s: result %+v, want one fetched and %d changed", s.name, res, s.changed)
		}
		if got, want := p.cachedAtB(t), s.want(); !slices.Equal(got, want) {
			t.Fatalf("%s: B caches %d tokens, A serves %d; want B to mirror A", s.name, len(got), len(want))
		}
		if got := p.cachedEvents(t); !slices.Equal(got, s.events) {
			t.Fatalf("%s: lease.cached events %+v, want %+v", s.name, got, s.events)
		}
	}
	if got := p.cachedAtB(t); len(got) != 1 {
		t.Fatalf("after E's revocation B caches %d, want D's one lease", len(got))
	}
}

// An unreachable sibling is ordinary: the pass does not fail, B keeps what it
// cached from A, nothing is recorded, and the next fetch that reaches A
// catches up.
func TestAnUnreachableSiblingKeepsItsCacheUntilAFetchSucceeds(t *testing.T) {
	t.Parallel()
	p := newPair(t)
	p.devices.set(deviceD)
	p.reconcileA(t)
	p.sync(t)
	before := p.cachedAtB(t)
	if len(before) != 1 {
		t.Fatalf("B caches %d after the first fetch, want 1", len(before))
	}

	// A changes, and B cannot reach it.
	p.devices.set(deviceD, deviceE)
	p.reconcileA(t)
	p.sibs.set(leasesync.Target{Peer: p.a.peer, Endpoint: unreachable(t)})
	res := p.sync(t)
	if res.Deferred != 1 || res.Fetched != 0 || res.Changed != 0 {
		t.Fatalf("unreachable A: result %+v, want one deferred", res)
	}
	if got := p.cachedAtB(t); !slices.Equal(got, before) {
		t.Fatalf("unreachable A changed B's cache: %d tokens, want the %d it held", len(got), len(before))
	}
	if n := len(p.cachedEvents(t)); n != 1 {
		t.Fatalf("%d lease.cached events after the failed fetch, want only the first fetch's", n)
	}

	// A is reachable again.
	p.sibs.set(p.target)
	res = p.sync(t)
	if res.Fetched != 1 || res.Changed != 1 {
		t.Fatalf("reachable again: result %+v, want one fetched and changed", res)
	}
	if got, want := p.cachedAtB(t), p.servedByA(t); !slices.Equal(got, want) || len(got) != 2 {
		t.Fatalf("after A is back B caches %d, A serves %d; want both of A's leases", len(got), len(want))
	}
}

// A fetch at any moment around A's refresh caches a lease with at least
// DeviceWriteRefreshThreshold of life, even when A's last issuer pass ran up
// to DeviceWriteReissueSlack before the fetch: the issuer and the fetch are
// independent beats, so the test must not reconcile A just before each fetch
// (Codex review on #808).
func TestAFetchAroundTheRefreshCachesAtLeastTheThreshold(t *testing.T) {
	t.Parallel()
	reissueAt := start.Add(leases.DeviceWriteTTL - leases.DeviceWriteRefreshThreshold - leases.DeviceWriteReissueSlack)
	slack := leases.DeviceWriteReissueSlack
	cases := []struct {
		name     string
		issuerAt time.Time // A's last issuer pass
		at       time.Time // B's fetch, with no issuer pass in between
	}{
		{"pass just before the re-issue point, fetch a slack later", reissueAt.Add(-time.Second), reissueAt.Add(slack - 2*time.Second)},
		{"pass at the re-issue point, fetch at once", reissueAt, reissueAt},
		{"pass just after the re-issue point, fetch a slack later", reissueAt.Add(time.Second), reissueAt.Add(slack)},
		{"pass and fetch together, mid-life", reissueAt.Add(-time.Hour), reissueAt.Add(-time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newPair(t)
			p.devices.set(deviceD)
			p.reconcileA(t) // issued at start
			p.sync(t)

			p.clock.set(tc.issuerAt)
			p.reconcileA(t)
			p.clock.set(tc.at)
			p.sync(t)

			trust := grant.Keys{p.a.id: p.a.pub}
			req := grant.Request{
				Principal:  leases.DevicePrincipal(deviceD),
				Resource:   leases.SiteResource(p.b.id),
				Capability: grant.CapabilityWrite,
			}
			var longest time.Duration
			for _, token := range p.cachedAtB(t) {
				g, err := grant.Verify(token, trust, req, tc.at)
				if err != nil {
					t.Fatalf("a cached lease does not verify at B: %v", err)
				}
				longest = max(longest, g.ExpiresAt.Sub(tc.at))
			}
			if longest < leases.DeviceWriteRefreshThreshold {
				t.Fatalf("B's freshest cached lease has %v left, want at least %v", longest, leases.DeviceWriteRefreshThreshold)
			}
		})
	}
}
