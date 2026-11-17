package worker

import (
	"bytes"
	"errors"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// #800's prerequisite (ADR-0119 amendment, #833): a media node pulls a blob
// whose only record here came from a sibling's signed op.
//
// The sibling is a Partial Peer (ADR-0119), so no inventory ever told this node
// what it holds, and replicate_blob refused the blob as unknown. A recorded
// op-sourced row names the signer as the source, and the pull is the ordinary
// one: over the pinned peer surface, hashed on arrival, the size hint replaced
// by the verified length.
func TestReplicateBlobPullsAnOpSourcedRowFromAPartialSibling(t *testing.T) {
	f := newTransferFabric(t)
	ctx := t.Context()
	f.exec(`UPDATE peers SET mode = 'partial' WHERE id = ?`, f.sourceID)

	content := bytes.Repeat([]byte("rom!"), 3000)
	desc, err := f.sourceStore.Put(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	// The source holds the bytes and has no manifest: pulled whole (M5-05).
	f.sourceMans.hold(desc.Hash)

	// Without a row this node cannot pull it: the gap #800 names.
	err = f.run(desc.Hash)
	if !errors.Is(err, ErrUnknownReplicationTarget) || !errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("before an op names it: err = %v, want a permanent unknown-target refusal", err)
	}

	var key []byte
	if err := f.db.Reader().QueryRow(`SELECT public_key FROM peers WHERE id = ?`, f.sourceID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	// The op's size is a hint, deliberately wrong here so the correction shows.
	source, err := f.cat.RecordOpSourcedBlob(ctx, catalog.OpSourcedBlob{
		BlobHash: desc.Hash.String(), Size: desc.Size + 5, SignerKey: identity.FormatPublicKey(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	if source != f.sourceID {
		t.Fatalf("source = %q, want the signer %q", source, f.sourceID)
	}

	if err := f.run(desc.Hash); err != nil {
		t.Fatalf("an op-sourced row must pull like any other: %v", err)
	}
	if state, n, ok := f.replicaState(desc.Hash); !ok || state != "present" || n != desc.Size {
		t.Fatalf("this node's replica = %q/%d/%v, want present/%d", state, n, ok, desc.Size)
	}
	if f.sourceBlobs.requests.Load() == 0 {
		t.Fatal("the source served nothing; the bytes did not come from the signer")
	}
	if f.controllerHits.Load() != 0 {
		t.Fatalf("the controller carried %d requests", f.controllerHits.Load())
	}
	got, err := f.store.Has(ctx, desc.Hash)
	if err != nil || !got {
		t.Fatalf("store holds the blob = %v (%v), want true", got, err)
	}
	var size int64
	if err := f.db.Reader().QueryRow(`SELECT size FROM blobs WHERE hash = ?`, desc.Hash.String()).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if size != desc.Size {
		t.Fatalf("blobs.size = %d, want the verified %d in place of the hint", size, desc.Size)
	}
	var corrected int
	if err := f.db.Reader().QueryRow(`SELECT count(*) FROM events WHERE type = ? AND subject_id = ?`,
		events.TypeBlobSizeCorrected, desc.Hash.String()).Scan(&corrected); err != nil {
		t.Fatal(err)
	}
	if corrected != 1 {
		t.Fatalf("blob.size_corrected = %d, want 1", corrected)
	}
}

// An op-sourced row whose signer serves other bytes is the ordinary corrupt
// transfer: quarantined and not recorded (invariant 1, ADR-0018). The op's
// claim earned it a connection, nothing more.
func TestReplicateBlobDoesNotTrustAnOpSourcedClaim(t *testing.T) {
	f := newTransferFabric(t)
	ctx := t.Context()
	f.exec(`UPDATE peers SET mode = 'partial' WHERE id = ?`, f.sourceID)

	claimed, err := f.sourceStore.Put(ctx, bytes.NewReader(bytes.Repeat([]byte("a"), 4096)))
	if err != nil {
		t.Fatal(err)
	}
	f.sourceMans.hold(claimed.Hash)
	f.sourceBlobs.wrong = bytes.Repeat([]byte("b"), 4096)

	var key []byte
	if err := f.db.Reader().QueryRow(`SELECT public_key FROM peers WHERE id = ?`, f.sourceID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cat.RecordOpSourcedBlob(ctx, catalog.OpSourcedBlob{
		BlobHash: claimed.Hash.String(), Size: claimed.Size, SignerKey: identity.FormatPublicKey(key),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.run(claimed.Hash); err == nil {
		t.Fatal("bytes that are not the op's digest were accepted")
	}
	if state, _, _ := f.replicaState(claimed.Hash); state == "present" {
		t.Fatal("a replica was recorded present for bytes that did not verify")
	}
	if held, _ := f.store.Has(ctx, claimed.Hash); held {
		t.Fatal("the store holds the claimed digest after a corrupt transfer")
	}
}
