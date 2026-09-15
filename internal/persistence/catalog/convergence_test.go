package catalog_test

import (
	"context"
	"sort"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
)

// gapPairs renders a plan's gaps as "blob@peer" strings, sorted, so a test can
// compare the whole set as a value rather than by index.
func gapPairs(gaps []replication.Gap) []string {
	out := make([]string, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, g.BlobHash+"@"+g.PeerID)
	}
	sort.Strings(out)
	return out
}

// TestConvergenceUnionsPlacementPins is ADR-0096's replication half: a vault blob
// has no `assets` row, so the canonical-set diff never names it, and it reaches a
// peer ONLY because a placement pin says it should. The pin is unioned onto the
// diff per-(blob, peer): a pin to one peer produces a gap for that peer and for
// no other.
func TestConvergenceUnionsPlacementPins(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Two Full Peers: this node (created lazily by SelfPeer) and a remote one
	// (seedRemotePeer enrols `remotePeer` and returns this node's id).
	self := h.seedRemotePeer(t)

	// A vault blob: bytes with a blob row but NO asset, so the canonical set does
	// not contain it and the diff alone would produce nothing.
	blob := hashOf('a')
	h.seedBlobs(t, blob)

	// With no pin, convergence finds no gap for it — the proof that the union, not
	// the diff, is what carries a vault blob.
	plan, err := h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatalf("PlanPeerConvergence: %v", err)
	}
	if len(plan.Gaps) != 0 {
		t.Fatalf("a vault blob with no pin should produce no gaps, got %v", gapPairs(plan.Gaps))
	}

	// Pin it to the remote peer only.
	if err := h.cat.PinPlacement(ctx, blob, remotePeer); err != nil {
		t.Fatalf("PinPlacement: %v", err)
	}

	plan, err = h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatalf("PlanPeerConvergence: %v", err)
	}
	// Exactly one gap, and it names the pinned peer. NOT the self peer: a pin to
	// one peer must not fan the blob to every Full Peer, which is the thing
	// ADR-0096 forbids and unioning into the flat canonical set would do.
	if got, want := gapPairs(plan.Gaps), []string{blob + "@" + remotePeer}; !equalStrings(got, want) {
		t.Fatalf("pinned-blob gaps = %v, want %v (self is %s)", got, want, self)
	}
}

// TestConvergencePinToNonFullPeerIsIgnored: §34's placement policies are unbuilt,
// so a pin naming a peer that is not a Full Peer produces no gap — there is no
// policy that says a partial or cache peer should hold anything.
func TestConvergencePinToNonFullPeerIsIgnored(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.SelfPeer(ctx); err != nil {
		t.Fatal(err)
	}

	// A cache peer — enrolled, but not `full`.
	h.exec(t, `INSERT INTO peers (id, name, site, mode, is_self, created_at, enrolled_at)
		VALUES ('cache-peer', 'cache', 'site-c', 'cache', 0, ?, ?)`, stamp, stamp)

	blob := hashOf('b')
	h.seedBlobs(t, blob)
	if err := h.cat.PinPlacement(ctx, blob, "cache-peer"); err != nil {
		t.Fatalf("PinPlacement: %v", err)
	}

	plan, err := h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatalf("PlanPeerConvergence: %v", err)
	}
	if len(plan.Gaps) != 0 {
		t.Fatalf("a pin to a non-Full peer should produce no gap, got %v", gapPairs(plan.Gaps))
	}
}

// TestConvergencePinSatisfiedByPresentReplica: a pin whose peer already holds the
// blob is not a gap. The pin says where the blob belongs; a `present` replica says
// it is already there, and convergence has nothing to do.
func TestConvergencePinSatisfiedByPresentReplica(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.seedRemotePeer(t)

	blob := hashOf('c')
	h.seedBlobs(t, blob)
	if err := h.cat.PinPlacement(ctx, blob, remotePeer); err != nil {
		t.Fatalf("PinPlacement: %v", err)
	}
	// The remote peer already holds the pinned bytes.
	h.exec(t, `INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
		VALUES (?, ?, 'present', 1024, ?)`, blob, remotePeer, stamp)

	plan, err := h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatalf("PlanPeerConvergence: %v", err)
	}
	if len(plan.Gaps) != 0 {
		t.Fatalf("a pin the peer already satisfies should produce no gap, got %v", gapPairs(plan.Gaps))
	}
}

// TestConvergencePinRespectsScope: a cycle scoped to one peer produces pin gaps
// only for that peer, exactly as the canonical-set diff is scoped — a pin to a
// peer outside the scope is left for that peer's own cycle.
func TestConvergencePinRespectsScope(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)

	blob := hashOf('d')
	h.seedBlobs(t, blob)
	if err := h.cat.PinPlacement(ctx, blob, remotePeer); err != nil {
		t.Fatalf("PinPlacement: %v", err)
	}

	// Scoped to self: the pin names the remote peer, so this cycle sees no gap.
	plan, err := h.cat.PlanPeerConvergence(ctx, self)
	if err != nil {
		t.Fatalf("PlanPeerConvergence(self): %v", err)
	}
	if len(plan.Gaps) != 0 {
		t.Fatalf("a cycle scoped to self should not act on a pin to the remote peer, got %v", gapPairs(plan.Gaps))
	}

	// Scoped to the remote peer: the pin is in scope and produces its gap.
	plan, err = h.cat.PlanPeerConvergence(ctx, remotePeer)
	if err != nil {
		t.Fatalf("PlanPeerConvergence(remote): %v", err)
	}
	if got, want := gapPairs(plan.Gaps), []string{blob + "@" + remotePeer}; !equalStrings(got, want) {
		t.Fatalf("scoped-to-remote gaps = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
