package worker

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/rarebit-one/voidbind-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
)

// #658: replicate_blob failing forever on a FOREIGN KEY refusal.
//
// The production signature was one peer id — this node's own — and a blob the
// node already held: a vault upload recorded only a placement pin, so the
// catalogue had no `blobs` row, convergence read the self-pin as a gap, and the
// handler's already-held short-circuit tried to record a replica that
// `replicas.blob_hash` could never reference. Every cycle, for every vault blob.
//
// The peer half — a target peer that is no longer a member — is covered too,
// and both refusals are asserted to happen before a connection is opened.

// countingHandler builds the handler over a puller that counts being built, so "nothing was
// transferred" is an observation rather than an inference.
func (f *transferFabric) countingHandler(built *atomic.Int64) HandlerFunc {
	return ReplicateBlobHandler(TransferDeps{
		Catalog: f.cat,
		Store:   f.store,
		Puller: func() (*transfer.Puller, error) {
			built.Add(1)
			return f.puller, nil
		},
	})
}

// TestAHeldBlobWithNoRowIsAdoptedNotRefusedForever is the reproduction. Before
// the fix this run returned "catalog: recording the transferred replica of …:
// FOREIGN KEY constraint failed (787)", and the next cycle planned it again.
func TestAHeldBlobWithNoRowIsAdoptedNotRefusedForever(t *testing.T) {
	f := newTransferFabric(t)
	ctx := t.Context()

	// Exactly what a vault upload left before #658: bytes in this node's
	// store, a pin to this node, and no blobs row.
	desc, err := f.store.Put(ctx, bytes.NewReader(bytes.Repeat([]byte("v"), 4096)))
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO placement_pins (blob_hash, peer_id, created_at) VALUES (?, ?, ?)`,
		desc.Hash.String(), f.self, f.stamp)

	// The planner does offer it to this node: that is how the node heals.
	if !f.plansTransferTo(desc.Hash, f.self) {
		t.Fatal("convergence did not plan the self-pinned blob; the reproduction is not reproducing")
	}

	var built atomic.Int64
	if err := f.countingHandler(&built)(ctx, f.job(desc.Hash, f.self)); err != nil {
		t.Fatalf("a blob this node holds must be recorded, not refused: %v", err)
	}
	if state, n, ok := f.replicaState(desc.Hash); !ok || state != "present" || n != desc.Size {
		t.Fatalf("replica = %q/%d/%v, want present/%d", state, n, ok, desc.Size)
	}
	if built.Load() != 0 || f.sourceBlobs.requests.Load() != 0 {
		t.Fatalf("held bytes were fetched: puller built %d times, %d source requests",
			built.Load(), f.sourceBlobs.requests.Load())
	}
	if f.plansTransferTo(desc.Hash, f.self) {
		t.Fatal("the next cycle planned the same transfer again — the loop #658 describes")
	}

	// Idempotent: a duplicate job that was already queued is a no-op.
	if err := f.countingHandler(&built)(ctx, f.job(desc.Hash, f.self)); err != nil {
		t.Fatalf("a re-run must succeed: %v", err)
	}
}

// TestATransferToARemovedPeerFailsPermanentlyBeforeAnyTransfer: a job whose
// target peer is gone dies on its first attempt, with a cause that says so, and
// opens no connection. The same fabric then moves bytes for a known target, so
// the refusal is not a fabric that refuses everything.
func TestATransferToARemovedPeerFailsPermanentlyBeforeAnyTransfer(t *testing.T) {
	f := newTransferFabric(t)
	hash := f.seedBlob(bytes.Repeat([]byte("r"), 8192))

	var built atomic.Int64
	gone := "01990000-0000-7000-8000-00000000gone"
	err := f.countingHandler(&built)(t.Context(), f.job(hash, gone))
	assertPermanentUnknown(t, err)
	if built.Load() != 0 || f.sourceBlobs.requests.Load() != 0 {
		t.Fatalf("a transfer to a removed peer reached the network: puller built %d, %d requests",
			built.Load(), f.sourceBlobs.requests.Load())
	}

	if err := f.countingHandler(&built)(t.Context(), f.job(hash, f.self)); err != nil {
		t.Fatalf("control: the same fabric must transfer to a known target: %v", err)
	}
	if f.sourceBlobs.requests.Load() == 0 {
		t.Fatal("control: the known-target transfer made no request")
	}
}

// TestATransferOfAnUnknownBlobNotHeldFailsPermanently: a blob this node has no
// row for and does not hold has nothing to pull against and nowhere to record
// the outcome, even though a source could serve it.
func TestATransferOfAnUnknownBlobNotHeldFailsPermanently(t *testing.T) {
	f := newTransferFabric(t)
	desc, err := f.sourceStore.Put(t.Context(), bytes.NewReader(bytes.Repeat([]byte("u"), 8192)))
	if err != nil {
		t.Fatal(err)
	}

	var built atomic.Int64
	assertPermanentUnknown(t, f.countingHandler(&built)(t.Context(), f.job(desc.Hash, f.self)))
	if built.Load() != 0 || f.sourceBlobs.requests.Load() != 0 {
		t.Fatalf("an unrecordable transfer reached the network: puller built %d, %d requests",
			built.Load(), f.sourceBlobs.requests.Load())
	}
	if _, _, ok := f.replicaState(desc.Hash); ok {
		t.Fatal("an unrecordable transfer left a replica row")
	}
}

func assertPermanentUnknown(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, jobs.ErrPermanent) || !errors.Is(err, ErrUnknownReplicationTarget) {
		t.Fatalf("want a permanent unknown-target failure, got %v", err)
	}
}

func (f *transferFabric) plansTransferTo(hash hashing.Hash, peer string) bool {
	f.t.Helper()
	plan, err := f.cat.PlanPeerConvergence(f.t.Context(), "")
	if err != nil {
		f.t.Fatal(err)
	}
	for _, g := range plan.Gaps {
		if g.BlobHash == hash.String() && g.PeerID == peer {
			return true
		}
	}
	return false
}
