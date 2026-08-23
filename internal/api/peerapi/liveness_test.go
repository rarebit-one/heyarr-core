// Every response in this file is drained and closed by peerGet, which
// bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by peerGet
package peerapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/health"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// The peer surface as a WITNESS (#184).
//
// Liveness is observed rather than declared (internal/peer/health), and until
// now the only thing observing it was the client API's membership guard. A
// remote peer holds no bearer token and never reaches that guard: it talks to
// THIS surface, and only this surface. So in the topology M4 builds, the
// interaction that actually happens between two peers was the one interaction
// nothing recorded, and a remote peer's stored health could not leave
// `unknown` whatever it did.
//
// These tests run against a real migrated database, a real membership store
// and a real mTLS listener, because what is under test is the join: the key a
// handshake proved, looked up in the table the tracker writes.

// livenessFixture is a peer surface with a real health tracker behind it.
type livenessFixture struct {
	tracker *health.Tracker
	events  *events.Log
	members *membership.Store
	clock   *livenessClock
}

type livenessClock struct{ now time.Time }

func (c *livenessClock) Now() time.Time { return c.now }

var livenessStart = time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)

func newLivenessFixture(t *testing.T) *livenessFixture {
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
	clk := &livenessClock{now: livenessStart}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: log, PeerName: "site-a", PeerSite: "site-a", Clock: clk,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.SelfPeer(ctx); err != nil {
		t.Fatal(err)
	}
	members, err := membership.New(membership.Options{
		DB: db, Events: log, Clock: clk, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := health.New(health.Options{
		DB: db, Events: log, Clock: clk, Window: 10 * time.Minute,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &livenessFixture{tracker: tracker, events: log, members: members, clock: clk}
}

// enrol admits a peer the way an operator does, so the row under test carries
// the health column's real default rather than one this test chose.
func (f *livenessFixture) enrol(t *testing.T, node *peerNode) string {
	t.Helper()
	res, err := f.members.Register(context.Background(), membership.Registration{
		Name: node.name, Site: "site-b", Endpoint: "https://127.0.0.1:1", PublicKey: node.pub,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.Member.PeerID
}

func (f *livenessFixture) stateOf(t *testing.T, peerID string) health.Peer {
	t.Helper()
	p, err := f.tracker.Of(context.Background(), peerID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// healthChanges returns the peer.health_changed edges recorded for a peer, as
// (from, to) pairs.
func (f *livenessFixture) healthChanges(t *testing.T, peerID string) [][2]string {
	t.Helper()
	evs, err := f.events.Since(context.Background(), 0, []string{events.TypePeerHealthChanged}, 100)
	if err != nil {
		t.Fatal(err)
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
			t.Fatal(err)
		}
		out = append(out, [2]string{body.From, body.To})
	}
	return out
}

// serveWithLiveness is serve() with a liveness sink attached.
func serveWithLiveness(
	t *testing.T, self *peerNode, members mtls.Membership, liveness peerapi.Liveness,
) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		Liveness:   liveness,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
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
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutting the peer surface down: %v", err)
		}
	})
	return &listener{srv: srv, self: self, addr: srv.Addr(), logs: logs}
}

// THE TRANSITION. A remote peer that has spoken to NOTHING but the peer
// surface reaches `reachable`.
//
// It asserts `unknown` FIRST, and that assertion is not decoration: without it
// the test passes on a build where the column was already reachable, and it
// would be measuring the default rather than the transition. That is exactly
// how #184 survived M4 — every test still passed, because nothing ever looked
// at whether the value could move.
func TestAPeerThatSpeaksOnlyToThePeerSurfaceBecomesReachable(t *testing.T) {
	f := newLivenessFixture(t)
	nodeA := newPeerNode(t, "01990000-0000-7000-8000-0000000000a1", "site-a")
	nodeB := newPeerNode(t, "01990000-0000-7000-8000-0000000000b1", "site-b")
	peerB := f.enrol(t, nodeB)

	root := newTrustRoot(
		mtls.Peer{PeerID: peerB, Name: nodeB.name, PublicKey: nodeB.pub},
		nodeA.member(),
	)
	l := serveWithLiveness(t, nodeA, root, f.tracker)

	before := f.stateOf(t, peerB)
	if before.State != health.StateUnknown {
		t.Fatalf("health before any peer traffic = %q, want %q — the transition is the subject "+
			"of this test, and a peer that started reachable would prove nothing",
			before.State, health.StateUnknown)
	}
	if before.Seen() {
		t.Fatalf("last_seen_at is already set (%s) before the peer said anything", before.LastSeenAt)
	}

	// The only thing node B does: one request, on the peer surface, with no
	// bearer token anywhere near it.
	status, body, _, err := peerGet(t, dialler(t, nodeB, root), l.identityURL())
	if err != nil {
		t.Fatalf("the peer request failed: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}

	after := f.stateOf(t, peerB)
	if after.State != health.StateReachable {
		t.Fatalf("health after a peer-surface request = %q, want %q. This is #184: the surface a "+
			"remote peer actually speaks to must be one of liveness's writers",
			after.State, health.StateReachable)
	}
	if !after.LastSeenAt.Equal(f.clock.now) {
		t.Errorf("last_seen_at = %s, want %s — the answer routing acts on is WHEN, not just what",
			after.LastSeenAt, f.clock.now)
	}
	if got := f.healthChanges(t, peerB); len(got) != 1 || got[0] != [2]string{"unknown", "reachable"} {
		t.Errorf("peer.health_changed edges = %v, want exactly one unknown -> reachable "+
			"(every state transition emits an event, and exactly one — ADR-0009)", got)
	}
}

// The observation is edge-triggered, not a heartbeat. A peer mid-transfer
// makes a great many requests, and one write per request would put a heartbeat
// into the single-writer control plane and into the event log.
func TestRepeatedPeerRequestsEmitOneEdge(t *testing.T) {
	f := newLivenessFixture(t)
	nodeA := newPeerNode(t, "01990000-0000-7000-8000-0000000000a2", "site-a")
	nodeB := newPeerNode(t, "01990000-0000-7000-8000-0000000000b2", "site-b")
	peerB := f.enrol(t, nodeB)
	root := newTrustRoot(mtls.Peer{PeerID: peerB, Name: nodeB.name, PublicKey: nodeB.pub}, nodeA.member())
	l := serveWithLiveness(t, nodeA, root, f.tracker)

	client := dialler(t, nodeB, root)
	for range 5 {
		if status, body, _, err := peerGet(t, client, l.identityURL()); err != nil || status != http.StatusOK {
			t.Fatalf("status = %d, err = %v: %s", status, err, body)
		}
	}
	if got := f.stateOf(t, peerB).State; got != health.StateReachable {
		t.Fatalf("health = %q, want %q", got, health.StateReachable)
	}
	if got := f.healthChanges(t, peerB); len(got) != 1 {
		t.Errorf("peer.health_changed edges = %v, want exactly 1: five requests from a live peer "+
			"are one edge, not five", got)
	}
}

// Liveness is recorded AFTER admission and never before it. A key this fabric
// does not pin is not a peer whose liveness there is any business recording —
// and it cannot complete a handshake to try.
func TestAKeyThisFabricDoesNotPinRecordsNoLiveness(t *testing.T) {
	f := newLivenessFixture(t)
	nodeA := newPeerNode(t, "01990000-0000-7000-8000-0000000000a3", "site-a")
	nodeB := newPeerNode(t, "01990000-0000-7000-8000-0000000000b3", "site-b")
	stranger := newPeerNode(t, "01990000-0000-7000-8000-0000000000b4", "site-c")
	peerB := f.enrol(t, nodeB)
	// The stranger has a row — an operator typed it in — and the trust root
	// this listener consults does not carry its key.
	peerStranger := f.enrol(t, stranger)

	root := newTrustRoot(mtls.Peer{PeerID: peerB, Name: nodeB.name, PublicKey: nodeB.pub}, nodeA.member())
	l := serveWithLiveness(t, nodeA, root, f.tracker)

	if _, _, _, err := peerGet(t, dialler(t, stranger, newTrustRoot(nodeA.member())), l.identityURL()); err == nil {
		t.Fatal("a key this fabric does not pin completed a request")
	}
	if got := f.stateOf(t, peerStranger).State; got != health.StateUnknown {
		t.Errorf("the refused peer's health = %q, want %q — a refused connection is not an "+
			"observation of anything", got, health.StateUnknown)
	}
	if got := f.stateOf(t, peerB).State; got != health.StateUnknown {
		t.Errorf("an unrelated peer's health moved to %q on somebody else's refused connection", got)
	}
}

// A liveness sink that fails must not fail the request it was observing. The
// peer asked for something; whether this node managed to write down that it
// was up is this node's problem.
func TestAFailingLivenessSinkDoesNotFailThePeerRequest(t *testing.T) {
	nodeA := newPeerNode(t, "01990000-0000-7000-8000-0000000000a5", "site-a")
	nodeB := newPeerNode(t, "01990000-0000-7000-8000-0000000000b5", "site-b")
	root := newTrustRoot(nodeA.member(), nodeB.member())
	sink := &failingLiveness{}
	l := serveWithLiveness(t, nodeA, root, sink)

	status, body, _, err := peerGet(t, dialler(t, nodeB, root), l.identityURL())
	if err != nil {
		t.Fatalf("the peer request failed: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if got := sink.calls.Load(); got != 1 {
		t.Fatalf("the liveness sink was called %d times, want 1", got)
	}
	if !strings.Contains(l.logs.String(), "recording that a peer was seen failed") {
		t.Error("the failure was swallowed silently; it must be logged")
	}
}

// errFailingSink is what a broken liveness writer reports.
var errFailingSink = errors.New("the liveness sink is down")

type failingLiveness struct{ calls atomic.Int64 }

func (f *failingLiveness) Seen(context.Context, []byte) error {
	f.calls.Add(1)
	return errFailingSink
}
