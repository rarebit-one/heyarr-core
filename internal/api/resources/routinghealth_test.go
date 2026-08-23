// Every HTTP response here is closed by the harness's t.Cleanup.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/routing"
	"github.com/rarebit-one/heyarr-core/internal/peer/health"
)

// #146's read routing, driven by health that actually MOVES (#184).
//
// The routing tests next door set the health column with an INSERT. That
// proves the join between the column and the preference, and it is the right
// way to test the preference — but it cannot notice the failure #184 records:
// that in the topology M4 builds, nothing could write that column for a remote
// peer, so the input those tests exercise was, in production, pinned at
// `unknown` forever.
//
// So these drive the column the way production does — through the health
// tracker, from an observation and from a sweep — and then ask the routing
// endpoint the same questions. If the writers regress, these fail; the
// synthetic ones would not.

// healthWindow is the silence these tests measure against. Short enough to
// read, and nothing sleeps: the tracker's clock is injected.
const routingHealthWindow = 10 * time.Minute

type movingClock struct{ now time.Time }

func (c *movingClock) Now() time.Time { return c.now }

func (c *movingClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// trackerOver builds a real health tracker over the harness's database, so the
// column the API reads is written by the code production writes it with.
func trackerOver(t *testing.T, h *harness, clk *movingClock) *health.Tracker {
	t.Helper()
	tracker, err := health.New(health.Options{
		DB: h.db, Events: h.events, Clock: clk, Window: routingHealthWindow,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tracker
}

func answered(t *testing.T, tracker *health.Tracker, peerID string) {
	t.Helper()
	if err := tracker.Answered(context.Background(), peerID); err != nil {
		t.Fatal(err)
	}
}

func healthOf(t *testing.T, tracker *health.Tracker, peerID string) health.State {
	t.Helper()
	p, err := tracker.Of(context.Background(), peerID)
	if err != nil {
		t.Fatal(err)
	}
	return p.State
}

// The unhealthy-local-versus-healthy-remote case, reached without touching the
// health column by hand.
//
// It walks the whole arc in one test, because the arc is the point: two peers
// that nothing has heard from route to NOBODY, one observation makes the
// remote one a source, a second makes the local one win on locality, and
// silence past the window hands it back to the remote one.
func TestRoutingFollowsHealthTheTrackerActuallyMoved(t *testing.T) {
	h := newHarness(t).seed()
	clk := &movingClock{now: fixedTime}
	tracker := trackerOver(t, h, clk)

	// This node holds no copy, so the answer is about the other two.
	h.dropReplica(blob1Hash, peerID)
	// Enrolled and never heard from — which is the state every remote peer in
	// M4 was permanently in. The column's real default, spelled out.
	h.addPeer(peerCID, "peer-c", "site-a", "https://peer-c:7777", string(health.StateUnknown))
	h.addPeer(peerBID, "peer-b", "site-b", "https://peer-b:7777", string(health.StateUnknown))
	h.putReplica(blob1Hash, peerCID, "present")
	h.putReplica(blob1Hash, peerBID, "present")

	// assert_eq on the enum, never a substring: "unknown" and "unreachable"
	// share no prefix, but "not_satisfied" contains "satisfied" and that
	// shipped in this repository once.
	if got := healthOf(t, tracker, peerBID); got != health.StateUnknown {
		t.Fatalf("peer-b health = %q, want %q before anything observes it", got, health.StateUnknown)
	}
	if got := healthOf(t, tracker, peerCID); got != health.StateUnknown {
		t.Fatalf("peer-c health = %q, want %q before anything observes it", got, health.StateUnknown)
	}

	// State 0: nothing has been heard from, so nothing is a source. `unknown`
	// is deliberately not a synonym for reachable — and this is what read
	// routing was doing for every remote peer before #184.
	p, routed := h.routedPlan(t, asset1ID, device1ID)
	if routed.PeerID != "" {
		t.Fatalf("a peer nothing has ever heard from was routed to: %q", routed.PeerID)
	}
	if p.Decision != "unplayable" {
		t.Errorf("decision = %q, want unplayable", p.Decision)
	}
	for _, peer := range []string{peerBID, peerCID} {
		if got := routed.codesFor(peer); !hasCode(got, routing.RejectPeerUnhealthy) {
			t.Errorf("peer %s codes = %v, want %q", peer, got, routing.RejectPeerUnhealthy)
		}
	}

	// State 1: the REMOTE peer answers something — the observation the peer
	// surface now makes on every authenticated inbound request (#184).
	answered(t, tracker, peerBID)
	if got := healthOf(t, tracker, peerBID); got != health.StateReachable {
		t.Fatalf("peer-b health = %q, want %q after it answered", got, health.StateReachable)
	}

	_, routed = h.routedPlan(t, asset1ID, device1ID)
	if routed.PeerID != peerBID {
		t.Fatalf("routed to %q, want the remote peer %q: the only peer anything has heard from "+
			"is the one at the other site, and locality must not beat a peer that is down",
			routed.PeerID, peerBID)
	}
	if routed.Reason == nil || routed.Reason.Code != routing.SelectedCrossSiteFallback {
		t.Errorf("reason = %+v, want %q", routed.Reason, routing.SelectedCrossSiteFallback)
	}
	if got := routed.codesFor(peerCID); !hasCode(got, routing.RejectPeerUnhealthy) {
		t.Errorf("peer-c codes = %v, want %q", got, routing.RejectPeerUnhealthy)
	}

	// State 2: the LOCAL peer answers too, and §31's locality preference takes
	// over — which is the control that shows the previous answer was about
	// health rather than about this test's peer ordering.
	answered(t, tracker, peerCID)
	_, routed = h.routedPlan(t, asset1ID, device1ID)
	if routed.PeerID != peerCID {
		t.Fatalf("routed to %q, want the same-site peer %q once both are healthy",
			routed.PeerID, peerCID)
	}
	if routed.Reason == nil || routed.Reason.Code != routing.SelectedSiteLocal {
		t.Errorf("reason = %+v, want %q", routed.Reason, routing.SelectedSiteLocal)
	}

	// State 3: THE UNHEALTHY-LOCAL CASE, produced by silence rather than by an
	// UPDATE. The clock moves past the window, the remote peer answers again —
	// as a live peer talking to the peer surface would — and the sweep finds
	// the local one silent.
	clk.advance(routingHealthWindow + time.Minute)
	answered(t, tracker, peerBID)
	if _, err := tracker.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := healthOf(t, tracker, peerCID); got != health.StateUnreachable {
		t.Fatalf("peer-c health = %q, want %q after a window of silence", got, health.StateUnreachable)
	}
	if got := healthOf(t, tracker, peerBID); got != health.StateReachable {
		t.Fatalf("peer-b health = %q, want %q — it answered inside the window", got, health.StateReachable)
	}

	_, routed = h.routedPlan(t, asset1ID, device1ID)
	if routed.PeerID != peerBID {
		t.Fatalf("routed to %q, want the healthy remote peer %q — an unhealthy peer at the "+
			"client's own site must not win on locality", routed.PeerID, peerBID)
	}
	if got := routed.codesFor(peerCID); !hasCode(got, routing.RejectPeerUnhealthy) {
		t.Errorf("peer-c codes = %v, want %q", got, routing.RejectPeerUnhealthy)
	}
}
