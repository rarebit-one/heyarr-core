package catalog_test

import (
	"context"
	"testing"
)

// The catalogue's half of #658: a replica can only be recorded for a blob and a
// peer it has rows for, so the planner must not plan — and a vault upload must
// not leave — a (blob, peer) that ends in a refused insert.

// TestAPinForABlobWithNoRowIsPlannedOnlyForThisNode: pins carry no foreign keys
// (migration 00050), so one can name a blob the catalogue never recorded. For
// another peer the transfer could never be recorded and is not planned; for this
// node it is, because the bytes may already be here and the handler adopts them.
func TestAPinForABlobWithNoRowIsPlannedOnlyForThisNode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)

	unknown := hashOf('d') // pinned, and NO blobs row
	known := hashOf('e')   // pinned, with a row: the control
	h.seedBlobs(t, known)
	for _, pin := range [][2]string{{unknown, remotePeer}, {unknown, self}, {known, remotePeer}} {
		if err := h.cat.PinPlacement(ctx, pin[0], pin[1]); err != nil {
			t.Fatal(err)
		}
	}

	plan, err := h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{unknown + "@" + self, known + "@" + remotePeer} // gapPairs sorts
	if got := gapPairs(plan.Gaps); !equalStrings(got, want) {
		t.Fatalf("gaps = %v, want %v", got, want)
	}
	if plan.Unrecordable != 1 {
		t.Fatalf("Unrecordable = %d, want 1 (the unknown blob pinned to the remote peer)", plan.Unrecordable)
	}
}

// TestAPinNamingARemovedPeerPlansNothing: the planner's peers come from the
// membership table, so removing a peer removes every gap to it — including a
// pin's, which has no foreign key to cascade.
func TestAPinNamingARemovedPeerPlansNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	h.seedRemotePeer(t)
	blob := hashOf('f')
	h.seedBlobs(t, blob)
	if err := h.cat.PinPlacement(ctx, blob, remotePeer); err != nil {
		t.Fatal(err)
	}

	plan, err := h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gapPairs(plan.Gaps), []string{blob + "@" + remotePeer}; !equalStrings(got, want) {
		t.Fatalf("before removal: gaps = %v, want %v", got, want)
	}

	h.exec(t, `DELETE FROM peers WHERE id = ?`, remotePeer)
	plan, err = h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Gaps) != 0 {
		t.Fatalf("after removal: gaps = %v, want none", gapPairs(plan.Gaps))
	}
}

// TestReplicationTargetNamesWhatIsMissing is the handler's precondition.
func TestReplicationTargetNamesWhatIsMissing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	h.seedRemotePeer(t)
	blob := hashOf('a')
	h.seedBlobs(t, blob)

	for _, tc := range []struct {
		name, blob, peer string
		peerKnown, known bool
	}{
		{"both known", blob, remotePeer, true, true},
		{"peer removed", blob, "01990000-0000-7000-8000-00000000gone", false, true},
		{"blob unknown", hashOf('b'), remotePeer, true, false},
	} {
		p, b, err := h.cat.ReplicationTarget(ctx, tc.blob, tc.peer)
		if err != nil {
			t.Fatal(err)
		}
		if p != tc.peerKnown || b != tc.known {
			t.Errorf("%s: peerKnown=%v blobKnown=%v, want %v %v", tc.name, p, b, tc.peerKnown, tc.known)
		}
	}
}

// TestRecordVaultBlobMakesTheUploadAReplica: the vault upload records a blob row,
// this node's present replica and the pin together, so convergence sees the
// bytes as held rather than as a gap, and a re-upload changes and emits nothing.
func TestRecordVaultBlobMakesTheUploadAReplica(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	blob := hashOf('c')

	before := h.eventCount(t)
	if err := h.cat.RecordVaultBlob(ctx, blob, 4096, self); err != nil {
		t.Fatal(err)
	}
	if got := h.eventCount(t) - before; got != 2 {
		t.Fatalf("recording a new vault blob emitted %d events, want 2 (blob.created, replica.present)", got)
	}

	var size int64
	if err := h.db.Reader().QueryRow(`SELECT size FROM blobs WHERE hash = ?`, blob).Scan(&size); err != nil {
		t.Fatalf("no blob row: %v", err)
	}
	var state string
	if err := h.db.Reader().QueryRow(`SELECT state FROM replicas WHERE blob_hash = ? AND peer_id = ?`,
		blob, self).Scan(&state); err != nil || state != "present" || size != 4096 {
		t.Fatalf("replica state=%q size=%d err=%v, want present 4096", state, size, err)
	}
	if got := refCount(t, h, blob); got != 1 {
		t.Fatalf("the pin should be the blob's one reference, got %d", got)
	}

	plan, err := h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range plan.Gaps {
		if g.PeerID == self {
			t.Fatalf("this node holds the vault blob and still planned a transfer to itself: %v", gapPairs(plan.Gaps))
		}
	}

	mid := h.eventCount(t)
	if err := h.cat.RecordVaultBlob(ctx, blob, 4096, self); err != nil {
		t.Fatal(err)
	}
	if got := h.eventCount(t) - mid; got != 0 {
		t.Fatalf("a re-upload emitted %d events, want 0", got)
	}
}
