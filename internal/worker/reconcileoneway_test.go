package worker

import (
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// The one-way reachability deadlock, reproduced against the real reconciler
// (#186, ADR-0037).
//
// # What this file exists to prove
//
// Nothing here is a bug in the reconciler, and nothing here changes when the
// enrolment check lands. It is a CHARACTERISATION: the silence #186 observed
// on real hardware is a property of the fabric's two flows running in opposite
// directions, it is reproducible without a network, and it is what an operator
// gets today instead of an error.
//
// The fabric: two Full Peers, one blob, and a link that carries traffic in one
// direction only. This node (the destination) lost its bytes; the other peer
// holds them and cannot say so, because an inventory report travels peer →
// controller and that is the direction the network forbids.
//
// The assertion is the absence: zero replicate_blob jobs, and the reconciler
// is RIGHT to emit none. It holds one replica row — its own, missing — and
// nothing has ever told it the other peer holds the bytes. A reconciler that
// enqueued a transfer here would be naming a source it invented.
//
// Read together with TestEnrolmentRefusesAOneWayPairing in internal/cli: the
// fix for this is not to make the reconciler louder, it is to stop the pairing
// being created at all. The silence stays; the operator stops meeting it
// weeks later as the first symptom.
func TestOneWayPairingReconcilesToSilence(t *testing.T) {
	h := newConvergeHarness(t)
	h.managed(t, blobOne)

	// The other peer holds the bytes. It cannot report that: its inventory
	// report is a POST to this controller, and the return path does not
	// exist. So h.reports is deliberately NOT called for h.other — the
	// omission IS the one-way link, expressed as the row that never arrives.

	// This node then loses its own copy, exactly as the fsck run in #186 did:
	// the replica goes to `missing` and the asset with it.
	if err := h.cat.MarkMissing(t.Context(), hashing.MustParse(blobOne), time.Now().UTC()); err != nil {
		t.Fatalf("marking this node's replica missing: %v", err)
	}

	summary := h.cycle(t, "", replicateBatch)

	// assert_eq on the counts, not a substring of a log line: the whole
	// finding is that this cycle reports a clean, successful, EMPTY pass.
	if got := len(h.transfers(t)); got != 0 {
		t.Fatalf("the reconciler emitted %d replicate_blob jobs under a one-way pairing, want 0 — "+
			"if this now emits work, #186's premise has changed and ADR-0037 needs revisiting", got)
	}
	if summary.Enqueued != 0 {
		t.Fatalf("the cycle reported %d enqueued, want 0", summary.Enqueued)
	}
	if summary.UnderReplicated != 0 {
		t.Fatalf("the cycle reported %d under-replicated pairs, want 0: the blob left the "+
			"canonical set with the asset, which is why the cycle is silent rather than blocked",
			summary.UnderReplicated)
	}
	// And the cycle succeeded. Silence, not failure, is the finding.
	if summary.Peers != 2 {
		t.Fatalf("the cycle considered %d Full Peers, want 2 — the fixture is not a two-peer fabric",
			summary.Peers)
	}
}
