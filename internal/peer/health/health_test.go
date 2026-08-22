package health_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/health"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Everything here runs against a real migrated SQLite database and a real
// event log. Health lives in two columns migration 00020 reserved, under a
// CHECK constraint, and the CHECK is half the model — a fake store would be
// testing this file's idea of the states rather than the schema's.
//
// Time is injected everywhere and nothing sleeps (house convention): the whole
// point of the window is that it is long, and a test that waited it out would
// either take fifteen minutes or prove something about a much shorter one.

// testWindow is short enough to read in an assertion and long enough that
// "just answered" and "silent past the window" are obviously different
// numbers. It is passed explicitly so no test depends on DefaultWindow.
const testWindow = 10 * time.Minute

var start = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

// answeringProber reports whatever it is told to, and counts the asking. It is
// how the "silence, not errors" rule is exercised without a socket: nil is an
// answer of any kind, an error is nothing coming back at all.
type answeringProber struct {
	err    error
	probes int
}

func (p *answeringProber) Probe(context.Context, health.Peer) error {
	p.probes++
	return p.err
}

type fixture struct {
	t      *testing.T
	db     *sqlite.DB
	log    *events.Log
	clock  *clock
	peers  *membership.Store
	selfID string
}

func newFixture(t *testing.T, prober health.Prober) (*fixture, *health.Tracker) {
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
	clk := &clock{now: start}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: log, PeerName: "this-node", PeerSite: "site-a", Clock: clk,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	selfID, err := cat.SelfPeer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	members, err := membership.New(membership.Options{
		DB: db, Events: log, Clock: clk, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := health.New(health.Options{
		DB: db, Events: log, Clock: clk, Window: testWindow, Prober: prober,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, db: db, log: log, clock: clk, peers: members, selfID: selfID}, tracker
}

// enrolWithKey is enrol, returning the key as well, for the tests that arrive
// through the request path rather than through a peer id.
func (f *fixture) enrolWithKey(name, endpoint string) (string, ed25519.PublicKey) {
	f.t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := f.peers.Register(context.Background(), membership.Registration{
		Name: name, Site: "site-b", Endpoint: endpoint, PublicKey: pub,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return res.Member.PeerID, pub
}

// enrol admits a peer the way an operator does, so the row under test is the
// row production creates — including its health column's default.
func (f *fixture) enrol(name, endpoint string) string {
	f.t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := f.peers.Register(context.Background(), membership.Registration{
		Name: name, Site: "site-b", Endpoint: endpoint, PublicKey: pub,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return res.Member.PeerID
}

// transitions returns the peer.health_changed events for one peer, oldest
// first, as (from, to) pairs.
func (f *fixture) transitions(peerID string) [][2]string {
	f.t.Helper()
	evs, err := f.log.Since(context.Background(), 0, []string{events.TypePeerHealthChanged}, 1000)
	if err != nil {
		f.t.Fatal(err)
	}
	var out [][2]string
	for _, e := range evs {
		if e.SubjectID != peerID {
			continue
		}
		var body struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if err := json.Unmarshal(e.Payload, &body); err != nil {
			f.t.Fatalf("peer.health_changed payload: %v", err)
		}
		out = append(out, [2]string{body.From, body.To})
	}
	return out
}

func stateOf(t *testing.T, tracker *health.Tracker, peerID string) health.State {
	t.Helper()
	p, err := tracker.Of(context.Background(), peerID)
	if err != nil {
		t.Fatal(err)
	}
	return p.State
}

func assertState(t *testing.T, tracker *health.Tracker, peerID string, want health.State, when string) {
	t.Helper()
	if got := stateOf(t, tracker, peerID); got != want {
		t.Errorf("%s: health = %q, want %q", when, got, want)
	}
}

func sweep(t *testing.T, tracker *health.Tracker) health.Summary {
	t.Helper()
	sum, err := tracker.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

// TestAPeerCrossesTheWindowFromHealthyToUnhealthy is the acceptance case, and
// it asserts BOTH halves from the SAME peer: a peer that has just answered is
// reachable, and after the window elapses it is not. One peer, one clock, no
// sleeping.
func TestAPeerCrossesTheWindowFromHealthyToUnhealthy(t *testing.T) {
	t.Parallel()
	// No prober: this is the pure timeout, with nothing asking on its behalf.
	f, tracker := newFixture(t, nil)
	ctx := context.Background()
	peerID := f.enrol("peer-b", "http://peer-b:7777")

	// Before anything: not reachable, and not unreachable either. An unprobed
	// peer has not been shown to be up (migration 00020).
	assertState(t, tracker, peerID, health.StateUnknown, "never heard from")
	if p, _ := tracker.Of(ctx, peerID); p.Seen() {
		t.Errorf("last_seen_at = %v, want zero for a peer nothing has heard from", p.LastSeenAt)
	}

	if err := tracker.Answered(ctx, peerID); err != nil {
		t.Fatal(err)
	}
	assertState(t, tracker, peerID, health.StateReachable, "just answered")
	if p, _ := tracker.Of(ctx, peerID); !p.LastSeenAt.Equal(start) {
		t.Errorf("last_seen_at = %v, want %v", p.LastSeenAt, start)
	}

	// Still inside the window. Silence is not yet evidence.
	f.clock.advance(testWindow)
	sweep(t, tracker)
	assertState(t, tracker, peerID, health.StateReachable, "silent for exactly the window")

	// Past it.
	f.clock.advance(time.Second)
	sweep(t, tracker)
	assertState(t, tracker, peerID, health.StateUnreachable, "silent past the window")

	// The timestamp survives the transition: it is the actionable half of the
	// verdict, and an unreachable peer whose last-seen was cleared is a status
	// nobody can act on.
	p, err := tracker.Of(ctx, peerID)
	if err != nil {
		t.Fatal(err)
	}
	if !p.LastSeenAt.Equal(start) {
		t.Errorf("last_seen_at after going unreachable = %v, want the moment it was last heard from (%v)",
			p.LastSeenAt, start)
	}
}

// TestAPeerReturningErrorsIsHealthy is the case the intuitive implementation
// gets backwards.
//
// The peer answers every probe with a 500. It is up: a process is listening,
// routing and replying, which is everything reachability means. Health keys on
// SILENCE, and there is none here.
//
// It uses the real HTTPProber against a real server rather than a stub,
// because the thing being asserted is that nothing on that path reads a status
// code.
func TestAPeerReturningErrorsIsHealthy(t *testing.T) {
	t.Parallel()
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "the peer is very busy and everything is going wrong", http.StatusInternalServerError)
	}))
	defer srv.Close()

	f, tracker := newFixture(t, health.HTTPProber{})
	peerID := f.enrol("peer-b", srv.URL)

	sweep(t, tracker)
	if requests == 0 {
		t.Fatal("the prober never asked the peer anything")
	}
	assertState(t, tracker, peerID, health.StateReachable,
		"a peer answering every request with a 500")

	// And it stays healthy across the window, because it keeps answering.
	f.clock.advance(testWindow + time.Second)
	sweep(t, tracker)
	assertState(t, tracker, peerID, health.StateReachable,
		"a peer that has answered 500s for longer than the window")

	if got := f.transitions(peerID); len(got) != 1 || got[0] != [2]string{"unknown", "reachable"} {
		t.Errorf("transitions = %v, want exactly one unknown->reachable — "+
			"an erroring peer is alive and must not flap", got)
	}
}

// TestSilenceIsWhatMakesAPeerUnreachable is the other half of the same rule,
// asserted through the same prober interface: a probe that returns an error
// (nothing came back) does NOT keep the peer alive.
func TestSilenceIsWhatMakesAPeerUnreachable(t *testing.T) {
	t.Parallel()
	prober := &answeringProber{err: context.DeadlineExceeded}
	f, tracker := newFixture(t, prober)
	peerID := f.enrol("peer-b", "http://peer-b:7777")

	if err := tracker.Answered(context.Background(), peerID); err != nil {
		t.Fatal(err)
	}
	assertState(t, tracker, peerID, health.StateReachable, "just answered")

	f.clock.advance(testWindow + time.Second)
	sweep(t, tracker)
	if prober.probes == 0 {
		t.Fatal("a peer past its window was never probed")
	}
	assertState(t, tracker, peerID, health.StateUnreachable, "probed, and nothing came back")
}

// TestTransitionsAreEdgeTriggered walks a peer down, up, down and up again and
// counts the events.
//
// The count is the assertion, not the existence: a health check that emitted on
// every probe would pass an "events exist" test while writing a heartbeat into
// the event log, which is the blob.verified mistake events.go:26 refused. So
// each phase is swept several times and each phase must contribute exactly one
// event.
func TestTransitionsAreEdgeTriggered(t *testing.T) {
	t.Parallel()
	prober := &answeringProber{err: context.DeadlineExceeded}
	f, tracker := newFixture(t, prober)
	ctx := context.Background()
	peerID := f.enrol("peer-b", "http://peer-b:7777")

	// Establish the starting edge, so the down-up-down-up sequence below is
	// counted from a settled reachable state rather than from 'unknown'.
	if err := tracker.Answered(ctx, peerID); err != nil {
		t.Fatal(err)
	}
	if got := len(f.transitions(peerID)); got != 1 {
		t.Fatalf("%d transitions after the first answer, want 1 (unknown->reachable)", got)
	}
	base := len(f.transitions(peerID))

	down := func(phase string) {
		f.clock.advance(testWindow + time.Second)
		before := len(f.transitions(peerID))
		for range 3 {
			sweep(t, tracker)
		}
		if got := len(f.transitions(peerID)) - before; got != 1 {
			t.Errorf("%s: %d events from three sweeps of a peer going down, want exactly 1", phase, got)
		}
		assertState(t, tracker, peerID, health.StateUnreachable, phase)
	}
	up := func(phase string) {
		before := len(f.transitions(peerID))
		for range 3 {
			if err := tracker.Answered(ctx, peerID); err != nil {
				t.Fatal(err)
			}
		}
		if got := len(f.transitions(peerID)) - before; got != 1 {
			t.Errorf("%s: %d events from three answers by a recovering peer, want exactly 1", phase, got)
		}
		assertState(t, tracker, peerID, health.StateReachable, phase)
	}

	down("first outage")
	up("first recovery")
	down("second outage")
	up("second recovery")

	got := f.transitions(peerID)[base:]
	want := [][2]string{
		{"reachable", "unreachable"},
		{"unreachable", "reachable"},
		{"reachable", "unreachable"},
		{"unreachable", "reachable"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d transitions across down-up-down-up, want %d: %v", len(got), len(want), got)
	}
	var toUnreachable, toReachable int
	for i, pair := range got {
		if pair != want[i] {
			t.Errorf("transition %d = %v, want %v", i, pair, want[i])
		}
		switch health.State(pair[1]) {
		case health.StateUnreachable:
			toUnreachable++
		case health.StateReachable:
			toReachable++
		case health.StateUnknown:
			t.Errorf("transition %d ends in %q, which nothing may transition INTO", i, pair[1])
		}
	}
	if toUnreachable != 2 || toReachable != 2 {
		t.Errorf("directions = %d down / %d up, want exactly one event per edge (2 and 2)",
			toUnreachable, toReachable)
	}
}

// TestAnUnreachablePeerIsSkippedAsASourceAndStillAccruesWorkAsADestination is
// the asymmetry this issue exists for.
//
// The second half is the one a plausible refactor breaks: Destinations looks
// like Sources with a filter missing. Work owed to a peer that is down stays
// owed, or the library quietly stops converging every time a site reboots.
func TestAnUnreachablePeerIsSkippedAsASourceAndStillAccruesWorkAsADestination(t *testing.T) {
	t.Parallel()
	f, tracker := newFixture(t, nil)
	ctx := context.Background()
	up := f.enrol("peer-up", "http://peer-up:7777")
	down := f.enrol("peer-down", "http://peer-down:7777")

	if err := tracker.Answered(ctx, up); err != nil {
		t.Fatal(err)
	}
	if err := tracker.Answered(ctx, down); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(testWindow + time.Second)
	if err := tracker.Answered(ctx, up); err != nil {
		t.Fatal(err)
	}
	sweep(t, tracker)
	assertState(t, tracker, up, health.StateReachable, "the peer that kept answering")
	assertState(t, tracker, down, health.StateUnreachable, "the peer that went quiet")

	peers, err := tracker.List(ctx)
	if err != nil {
		t.Fatal(err)
	}

	sources := ids(health.Sources(peers))
	if sources[down] {
		t.Error("an unreachable peer was offered as a read source; §31 serves content from HEALTHY peers")
	}
	if !sources[up] {
		t.Error("the reachable peer was not offered as a read source")
	}

	destinations := ids(health.Destinations(peers))
	if !destinations[down] {
		t.Error("an unreachable peer stopped accruing work as a replication destination. " +
			"Work owed to a peer that is down stays owed — filtering here means the library " +
			"reports itself converged every time a site reboots, and the gap is never noticed")
	}
	if !destinations[up] {
		t.Error("the reachable peer is not a destination")
	}
	if len(health.Destinations(peers)) != len(peers) {
		t.Errorf("Destinations dropped %d of %d peers; it must drop none",
			len(peers)-len(health.Destinations(peers)), len(peers))
	}
}

// TestAnUnknownPeerIsNotASource pins the migration's choice of default: a peer
// nothing has ever heard from is not routed to on an assumption.
func TestAnUnknownPeerIsNotASource(t *testing.T) {
	t.Parallel()
	f, tracker := newFixture(t, nil)
	peerID := f.enrol("peer-b", "http://peer-b:7777")
	peers, err := tracker.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ids(health.Sources(peers))[peerID] {
		t.Error("a peer that has never been heard from was offered as a read source")
	}
	if !ids(health.Destinations(peers))[peerID] {
		t.Error("a newly enrolled peer must still accrue the work it is owed")
	}
}

func ids(peers []health.Peer) map[string]bool {
	out := make(map[string]bool, len(peers))
	for _, p := range peers {
		out[p.PeerID] = true
	}
	return out
}

// TestDerive is the rule on its own: silence against a window, and nothing
// else. There is no error count in it, and a table here is the cheapest place
// to notice if one appears.
func TestDerive(t *testing.T) {
	t.Parallel()
	now := start
	for _, tc := range []struct {
		name     string
		lastSeen time.Time
		want     health.State
	}{
		{"never heard from", time.Time{}, health.StateUnknown},
		{"answered just now", now, health.StateReachable},
		{"answered a moment ago", now.Add(-time.Second), health.StateReachable},
		{"silent for exactly the window", now.Add(-testWindow), health.StateReachable},
		{"silent for a nanosecond past the window", now.Add(-testWindow - time.Nanosecond), health.StateUnreachable},
		{"silent all night", now.Add(-12 * time.Hour), health.StateUnreachable},
	} {
		if got := health.Derive(tc.lastSeen, now, testWindow); got != tc.want {
			t.Errorf("%s: Derive = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestTheSweepOnlyProbesPeersNothingHasHeardFrom pins decision 1: liveness
// comes from interactions that were going to happen anyway, and the probe is
// only for the idle case. A sweep that probed everything every beat would be a
// heartbeat Heyarr invented for itself.
func TestTheSweepOnlyProbesPeersNothingHasHeardFrom(t *testing.T) {
	t.Parallel()
	prober := &answeringProber{}
	f, tracker := newFixture(t, prober)
	ctx := context.Background()
	peerID := f.enrol("peer-b", "http://peer-b:7777")

	if err := tracker.Answered(ctx, peerID); err != nil {
		t.Fatal(err)
	}
	sweep(t, tracker)
	if prober.probes != 0 {
		t.Errorf("%d probes of a peer heard from moments ago, want 0", prober.probes)
	}

	f.clock.advance(testWindow/2 + time.Second)
	sweep(t, tracker)
	if prober.probes != 1 {
		t.Errorf("%d probes of a peer idle for over half the window, want 1", prober.probes)
	}
	// The probe answered, so the peer never crossed the edge.
	assertState(t, tracker, peerID, health.StateReachable, "probed and answering")
	if got := len(f.transitions(peerID)); got != 1 {
		t.Errorf("%d transitions, want 1 (the first answer) — a probe that answers is not an edge", got)
	}
}

// TestAPeerWithNoEndpointIsNotProbed keeps a configuration gap from being
// reported as an outage.
func TestAPeerWithNoEndpointIsNotProbed(t *testing.T) {
	t.Parallel()
	prober := &answeringProber{}
	f, tracker := newFixture(t, prober)
	f.enrol("peer-b", "")
	f.clock.advance(testWindow + time.Second)
	sweep(t, tracker)
	if prober.probes != 0 {
		t.Errorf("%d probes of a peer with no endpoint, want 0", prober.probes)
	}
}

// TestSelfIsSeenByTheSweep. The process running the sweep is the evidence, so
// the one peer this node is certain about must not read as "never heard from".
func TestSelfIsSeenByTheSweep(t *testing.T) {
	t.Parallel()
	prober := &answeringProber{err: context.DeadlineExceeded}
	f, tracker := newFixture(t, prober)
	sweep(t, tracker)
	assertState(t, tracker, f.selfID, health.StateReachable, "this node, after a sweep")
	if prober.probes != 0 {
		t.Errorf("%d probes of this node's own listener, want 0", prober.probes)
	}
}

// TestTheEventCarriesBothEndsAndTheWindow. peer.health_changed is one type
// rather than a peer.up/peer.down pair, so a subscriber that wants only one
// direction reassembles it from the payload — which means the payload has to
// carry it.
func TestTheEventCarriesBothEndsAndTheWindow(t *testing.T) {
	t.Parallel()
	f, tracker := newFixture(t, nil)
	ctx := context.Background()
	peerID := f.enrol("peer-b", "http://peer-b:7777")
	if err := tracker.Answered(ctx, peerID); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(testWindow + time.Second)
	sweep(t, tracker)

	evs, err := f.log.Since(ctx, 0, []string{events.TypePeerHealthChanged}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var last events.Event
	for _, e := range evs {
		if e.SubjectID == peerID {
			last = e
		}
	}
	if last.Type != events.TypePeerHealthChanged {
		t.Fatalf("no peer.health_changed for %s", peerID)
	}
	var body struct {
		PeerID     string  `json:"peer_id"`
		Name       string  `json:"name"`
		From       string  `json:"from"`
		To         string  `json:"to"`
		Window     string  `json:"window"`
		LastSeenAt *string `json:"last_seen_at"`
	}
	if err := json.Unmarshal(last.Payload, &body); err != nil {
		t.Fatal(err)
	}
	if body.From != string(health.StateReachable) {
		t.Errorf("from = %q, want %q", body.From, health.StateReachable)
	}
	if body.To != string(health.StateUnreachable) {
		t.Errorf("to = %q, want %q", body.To, health.StateUnreachable)
	}
	if body.Name != "peer-b" {
		t.Errorf("name = %q, want %q", body.Name, "peer-b")
	}
	if body.Window != testWindow.String() {
		t.Errorf("window = %q, want %q", body.Window, testWindow)
	}
	if body.LastSeenAt == nil {
		t.Fatal("last_seen_at is null on a transition into unreachable; " +
			"it is the half an operator acts on")
	}
	if *body.LastSeenAt != start.Format(time.RFC3339Nano) {
		t.Errorf("last_seen_at = %q, want %q", *body.LastSeenAt, start.Format(time.RFC3339Nano))
	}
	if last.SubjectType != "peer" {
		t.Errorf("subject_type = %q, want %q", last.SubjectType, "peer")
	}
}

// TestAProbeAnsweringRecoversAPeer closes the loop the other way: recovery does
// not need an inbound interaction, because the idle probe is an interaction.
func TestAProbeAnsweringRecoversAPeer(t *testing.T) {
	t.Parallel()
	prober := &answeringProber{err: context.DeadlineExceeded}
	f, tracker := newFixture(t, prober)
	peerID := f.enrol("peer-b", "http://peer-b:7777")

	if err := tracker.Answered(context.Background(), peerID); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(testWindow + time.Second)
	sum := sweep(t, tracker)
	if sum.BecameUnreachable != 1 {
		t.Errorf("BecameUnreachable = %d, want 1", sum.BecameUnreachable)
	}
	assertState(t, tracker, peerID, health.StateUnreachable, "after the outage")

	prober.err = nil
	sum = sweep(t, tracker)
	if sum.BecameReachable != 1 {
		t.Errorf("BecameReachable = %d, want 1", sum.BecameReachable)
	}
	assertState(t, tracker, peerID, health.StateReachable, "after the probe answered again")
}

// TestAnInboundPeerRequestIsLiveness is decision 1 where it matters most: the
// peer plane learns that a peer is up from work that was going to happen
// anyway, not from a probe Heyarr invented. A request arriving from a peer is
// that peer proving it is up, on a connection it opened.
func TestAnInboundPeerRequestIsLiveness(t *testing.T) {
	t.Parallel()
	f, tracker := newFixture(t, nil)
	ctx := context.Background()
	peerID, pub := f.enrolWithKey("peer-b", "http://peer-b:7777")

	assertState(t, tracker, peerID, health.StateUnknown, "before any request")
	if err := tracker.Seen(ctx, pub); err != nil {
		t.Fatal(err)
	}
	assertState(t, tracker, peerID, health.StateReachable, "after a request from the peer")

	// It is a peer that went quiet and came back, with no probe involved.
	f.clock.advance(testWindow + time.Second)
	sweep(t, tracker)
	assertState(t, tracker, peerID, health.StateUnreachable, "after the window with no requests")
	if err := tracker.Seen(ctx, pub); err != nil {
		t.Fatal(err)
	}
	assertState(t, tracker, peerID, health.StateReachable, "after the peer talked to us again")
	if got := len(f.transitions(peerID)); got != 3 {
		t.Errorf("%d transitions, want 3 (unknown->reachable, ->unreachable, ->reachable)", got)
	}
}

// TestLivenessRecordingIsThrottled. Seen runs on every request a peer makes,
// and a peer mid-transfer makes a great many; each one must not become a write
// to the single-writer control plane to move a timestamp by milliseconds.
//
// The throttle is a tenth of the window, which cannot flip a peer across the
// edge — so what is asserted is that the write is skipped, and that the answer
// is still right.
func TestLivenessRecordingIsThrottled(t *testing.T) {
	t.Parallel()
	f, tracker := newFixture(t, nil)
	ctx := context.Background()
	peerID, pub := f.enrolWithKey("peer-b", "http://peer-b:7777")

	if err := tracker.Seen(ctx, pub); err != nil {
		t.Fatal(err)
	}
	first, err := tracker.Of(ctx, peerID)
	if err != nil {
		t.Fatal(err)
	}

	// Well inside the resolution: the fact is already recorded closely enough.
	f.clock.advance(testWindow/20 + time.Second)
	for range 50 {
		if err := tracker.Seen(ctx, pub); err != nil {
			t.Fatal(err)
		}
	}
	after, err := tracker.Of(ctx, peerID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastSeenAt.Equal(first.LastSeenAt) {
		t.Errorf("last_seen_at moved to %v on a request inside the resolution; "+
			"fifty requests must not be fifty writes", after.LastSeenAt)
	}
	if after.State != health.StateReachable {
		t.Errorf("health = %q while being talked to constantly, want reachable", after.State)
	}

	// Past the resolution: it is recorded again.
	f.clock.advance(testWindow / 5)
	if err := tracker.Seen(ctx, pub); err != nil {
		t.Fatal(err)
	}
	moved, err := tracker.Of(ctx, peerID)
	if err != nil {
		t.Fatal(err)
	}
	if !moved.LastSeenAt.After(first.LastSeenAt) {
		t.Errorf("last_seen_at = %v, want a later time — the throttle must not stop the record entirely",
			moved.LastSeenAt)
	}
	// And exactly one transition across all of it: none of those requests was
	// an edge.
	if got := len(f.transitions(peerID)); got != 1 {
		t.Errorf("%d transitions from a peer that was up the whole time, want 1", got)
	}
}

// TestSeenIgnoresAKeyThatIsNotAMember. Revocation is deletion (ADR-0012), so a
// row disappearing between the membership guard and this call is the supported
// race, not a failure — and it must not be able to fail the request it happened
// during.
func TestSeenIgnoresAKeyThatIsNotAMember(t *testing.T) {
	t.Parallel()
	_, tracker := newFixture(t, nil)
	stranger, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Seen(context.Background(), stranger); err != nil {
		t.Errorf("Seen on a non-member = %v, want nil", err)
	}
}
