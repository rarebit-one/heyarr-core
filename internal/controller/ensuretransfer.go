package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// blobTransferEnsurer is the client blob route's [blobs.TransferEnsurer]: it
// gates a GET on the blob being desired and, if it is, ensures the transfer that
// fetches it is enqueued — through the job table (#371, Option A).
//
// # Why this lives in the controller, not the blob package
//
// The blob-serving package speaks bytes and knows nothing of wants, placement or
// the job queue (ADR-0013). Deciding "is this blob desired?" is a catalog
// question and starting a transfer is a job-table write, so both belong on this
// side of the boundary — the same side that decodes piece geometry for the
// partial route (piecePartialSource). The route calls one interface method and
// stays free of all of it.
//
// # invariant 4, and invariant 9
//
// The enqueue is a replicate_blob row, never an in-process call into the worker:
// the API role reaches the worker only through the job table (invariant 4). It
// is idempotent (invariant 9), keyed on blob + destination — the SAME key
// reconcile_peer uses for a gap this node must close — so a GET, a concurrent
// GET, and a reconciliation cycle that all want these bytes here collapse to one
// transfer rather than stacking duplicates.
type blobTransferEnsurer struct {
	cat   *catalog.Catalog
	queue *jobs.Queue
	log   *slog.Logger
}

var _ blobs.TransferEnsurer = blobTransferEnsurer{}

// EnsureTransfer answers the gate and, only when it holds, enqueues the pull.
//
// The destination is THIS node: replication is a destination pull (ADR-0030), so
// the machine that will hold the bytes is the one that fetches them, and a GET
// arriving here is a request for these bytes to be here. No source is named —
// the transfer chooses one when it runs, which may not be the best source now
// (the same reason ReplicateBlobPayload carries none).
func (e blobTransferEnsurer) EnsureTransfer(ctx context.Context, blob hashing.Hash) (bool, error) {
	desired, err := e.cat.IsBlobDesired(ctx, blob.String())
	if err != nil {
		return false, err
	}
	if !desired {
		// The gate: nobody has decided they want this hash, so a GET for it must
		// not make this node fetch it. The route answers 404.
		return false, nil
	}

	self, err := e.cat.SelfPeer(ctx)
	if err != nil {
		// Desired, but this node cannot name itself as the destination — surface
		// it rather than silently declining to start the transfer.
		return true, err
	}

	gap := replication.Gap{BlobHash: blob.String(), PeerID: self}
	if _, err := e.queue.Enqueue(ctx, jobs.EnqueueOptions{
		Type: replication.ReplicateBlobJobType,
		Payload: replication.ReplicateBlobPayload{
			BlobHash:          blob.String(),
			DestinationPeerID: self,
		},
		// blob + destination, the key reconcile_peer uses (Gap.DedupeKey). The
		// queue's partial-unique index over live jobs turns a second enqueue —
		// another GET, or a reconciliation cycle — into a no-op that returns the
		// job already in flight, so a player reaching for content a reconcile is
		// already fetching does not start a second copy of the same transfer.
		DedupeKey: gap.DedupeKey(),
	}); err != nil {
		return true, fmt.Errorf("controller: ensuring a transfer for desired blob %s: %w", blob, err)
	}
	e.log.Debug("ensured a transfer for a desired blob a client reached for",
		"blob_hash", blob.String(), "destination_peer_id", self)
	return true, nil
}
