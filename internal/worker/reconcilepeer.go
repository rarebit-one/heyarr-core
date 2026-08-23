package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// replicateBatch bounds how many replicate_blob jobs one cycle may create.
//
// A first sync between two Full Peers is the case this number exists for. A
// library of a hundred thousand blobs would otherwise put a hundred thousand
// pending jobs in the queue in one cycle: a queue no operator can see past, a
// jobs table that dwarfs the catalog, and every other job type starved behind
// work that will take days regardless.
//
// It costs nothing in convergence rate. Transfers are the bottleneck, not job
// rows — the next cycle takes the next slice, and the fabric converges at
// exactly the speed the network allows either way. What the bound buys is that
// the queue stays a thing a human can read.
//
// Two hundred and fifty is chosen against the beat: at one cycle every five
// minutes it offers three thousand transfers an hour, which is far more than a
// homelab link will complete, so the bound is never the limiting factor in
// practice — it only stops the pathological first cycle.
const replicateBatch = 250

// ReconcilePeerHandler is §57's peer-convergence reconciliation (§19, M4-08).
//
// It diffs §19's desired blob set against what the peers have reported holding
// and emits replicate_blob work for the difference. It EMITS the work and
// stops there: moving the bytes is M4-09, and a reconciler that also
// transferred would be a reconciler whose cycle time depended on the size of
// the library.
//
// # Idempotent, and monotonic toward the desired set (invariant 9)
//
// It will be re-run. Two things make that free rather than merely survivable:
//
//   - The diff is PURE. It holds no cursor and remembers no previous cycle, so
//     the second run over an unchanged fabric computes the same gaps, finds
//     each of them already in flight, and creates nothing.
//   - Every job it creates is keyed on blob_hash + destination peer, so even
//     if the in-flight check were wrong — or two cycles raced — the queue's
//     partial-unique index over live jobs would refuse the duplicate. The key
//     is the guarantee; the in-flight check is bookkeeping.
//
// The property worth stating, because a single run cannot show it: a
// reconciler that re-enqueues the same transfer every cycle is not converging,
// it is looping, and the two are indistinguishable in one pass. Across cycles
// they are not — the work count of a converging system falls, and only ever
// falls, as inventories report the transfers landing.
func ReconcilePeerHandler(cat *catalog.Catalog, queue *jobs.Queue, log *slog.Logger) HandlerFunc {
	return reconcilePeerHandler(cat, queue, log, replicateBatch)
}

// ReconcilePeerRegistration is how this job is registered, as one value.
//
// It is a function rather than a literal in worker.go so that the two
// properties the registration IS — one cycle at a time, and no required
// capability — can be asserted rather than read. A registration written inline
// at the call site is a registration whose capability requirement can be added
// later with nothing to notice.
//
// No RequiredCapability: the cycle needs nothing but the database — no
// toolchain, no indexer, no download client, and not even a reachable peer,
// since the diff is against the last inventory a peer reported rather than
// against a live probe. Following the precedent set by reconcile_desired, a
// fully degraded node still knows what it is missing, which is exactly the
// node whose operator most needs to be told.
//
// MaxConcurrent 1: two concurrent cycles would each read the fabric while the
// other enqueued against it, and the loser would spend the pass deciding
// against a picture that had already moved.
func ReconcilePeerRegistration(cat *catalog.Catalog, queue *jobs.Queue, log *slog.Logger) Registration {
	return Registration{
		Handler:       ReconcilePeerHandler(cat, queue, log),
		MaxConcurrent: 1,
	}
}

// reconcilePeerHandler is the handler with its per-cycle bound as a parameter.
//
// The bound is a parameter rather than only a constant because "what was
// deferred is reported, and a later cycle picks it up" is a property of the
// mechanism and not of the number — and a test that had to enqueue
// replicateBatch+1 transfers to reach it would be a test nobody runs.
func reconcilePeerHandler(
	cat *catalog.Catalog, queue *jobs.Queue, log *slog.Logger, limit int,
) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		var payload replication.ReconcilePeerPayload
		if len(job.Payload) > 0 {
			if err := json.Unmarshal(job.Payload, &payload); err != nil {
				return fmt.Errorf("worker: reconcile_peer payload is not decodable: %w", err)
			}
		}

		plan, err := cat.PlanPeerConvergence(ctx, payload.PeerID)
		if err != nil {
			return err
		}

		summary := catalog.PeerReconcileSummary{
			Scope:           plan.Scope,
			Peers:           len(plan.Peers),
			Desired:         plan.Desired,
			UnderReplicated: len(plan.Gaps),
		}

		// What is already queued or running. Gaps covered by a live job are
		// not work this cycle can do anything about, and — the part that
		// matters for the bound — counting them against the batch would mean
		// the same first replicateBatch gaps were re-offered every cycle and
		// the remainder was never reached.
		live, err := queue.LiveDedupeKeys(ctx, replication.ReplicateBlobJobType)
		if err != nil {
			return err
		}
		outstanding := make([]replication.Gap, 0, len(plan.Gaps))
		for _, gap := range plan.Gaps {
			if _, inFlight := live[gap.DedupeKey()]; inFlight {
				summary.InFlight++
				continue
			}
			outstanding = append(outstanding, gap)
		}

		taken, deferred := replication.Bound(outstanding, limit)
		summary.Deferred = deferred

		// §16's trigger, read literally: "when replication or deduplication
		// requires it". This cycle has just decided that these blobs are about
		// to cross a network, which is the one thing that makes a manifest
		// worth the read it costs (M5-04) — a resumed transfer re-fetches a
		// chunk instead of a 20 GB remux, and a destination that already holds
		// some of those chunks fetches fewer still (ADR-0035, ADR-0036).
		//
		// This is NOT a sweep, and the difference is the whole of §16. Nothing
		// here walks the store: the work is bounded by the same batch the
		// transfers are bounded by, and a library nobody is replicating is a
		// library that never gets chunked at all. A blob under the size
		// threshold costs a Stat and a recorded exemption, not a read.
		//
		// The STATE is read to decide, and reading it generates nothing
		// (ADR-0034) — one query for the whole catalog, and only `undecided`
		// produces a job. The dedupe key would collapse the duplicates anyway;
		// this keeps the cycle from offering work that is already answered.
		chunkStates, err := cat.ChunkManifestStates(ctx)
		if err != nil {
			return err
		}
		chunkAsked := make(map[string]struct{}, len(taken))

		for _, gap := range taken {
			if err := ctx.Err(); err != nil {
				// An ordinary shutdown mid-cycle. What was enqueued stays
				// enqueued and the rest stays a gap, which the next cycle
				// recomputes from scratch — the benefit of the diff being pure
				// rather than a consumed work list.
				break
			}
			if _, err := queue.Enqueue(ctx, jobs.EnqueueOptions{
				Type: replication.ReplicateBlobJobType,
				Payload: replication.ReplicateBlobPayload{
					BlobHash:          gap.BlobHash,
					DestinationPeerID: gap.PeerID,
				},
				// blob_hash + destination peer. The queue's unique index over
				// live jobs turns this into the idempotency, so a concurrent
				// cycle — or a cycle whose in-flight read was stale — creates
				// nothing rather than a second transfer of the same bytes.
				//
				// No RequiredCapability, mirroring the search beat: routing
				// happens at registration, and stating it twice is two places
				// for the two statements to disagree.
				DedupeKey: gap.DedupeKey(),
			}); err != nil {
				if errors.Is(err, context.Canceled) {
					break
				}
				// One blob failing to enqueue must not stop the cycle. It
				// stays a gap and the next cycle offers it again.
				log.Warn("could not enqueue a replication",
					"blob_hash", gap.BlobHash, "peer_id", gap.PeerID, "error", err)
				continue
			}
			summary.Enqueued++

			// One chunk_blob per blob, not per gap: the same blob missing from
			// three peers is three transfers and one manifest. The dedupe key
			// enforces that across cycles; this map is what stops the same
			// cycle asking three times.
			if _, asked := chunkAsked[gap.BlobHash]; asked {
				continue
			}
			chunkAsked[gap.BlobHash] = struct{}{}
			if chunkStates[gap.BlobHash] != manifests.StateUndecided {
				continue
			}
			if _, err := queue.Enqueue(ctx, jobs.EnqueueOptions{
				Type:      manifests.ChunkBlobJobType,
				Payload:   manifests.ChunkBlobPayload{BlobHash: gap.BlobHash},
				DedupeKey: manifests.ChunkBlobDedupeKey(gap.BlobHash),
			}); err != nil {
				if errors.Is(err, context.Canceled) {
					break
				}
				// A manifest is an optimisation and never a precondition
				// (ADR-0034): a transfer with no manifest is a transfer that
				// moves the whole blob, which is what happens today. So this
				// failing must not fail the cycle, and must not hold up the
				// replication it accompanies.
				log.Warn("could not enqueue a chunking",
					"blob_hash", gap.BlobHash, "error", err)
			}
		}

		if summary.Deferred > 0 {
			// Logged rather than silently dropped. A cycle that hit its bound
			// has NOT converged, and without this line it looks exactly like
			// one that has: the queue is full of work, the cycle reported
			// success, and nothing says there is more behind it.
			log.Info("bounded a peer reconciliation cycle",
				"enqueued", summary.Enqueued, "deferred", summary.Deferred,
				"limit", limit,
				"note", "the remainder is taken by the next cycle")
		}
		if summary.Enqueued > 0 {
			log.Info("peer convergence reconciled",
				"peers", summary.Peers, "desired", summary.Desired,
				"under_replicated", summary.UnderReplicated,
				"in_flight", summary.InFlight, "enqueued", summary.Enqueued,
				"deferred", summary.Deferred)
		}

		if err := ctx.Err(); err != nil {
			// Stopped mid-cycle. The summary would describe a cycle that did
			// not finish, and emitting it against a cancelled context would
			// fail the job for an ordinary shutdown. The next cycle recomputes
			// everything from scratch.
			log.Info("peer reconciliation stopped early",
				"enqueued", summary.Enqueued, "reason", err)
			return nil
		}

		// One event per cycle, always — including the cycle that decided to do
		// nothing, which is the outcome that leaves no job rows behind to
		// prove it happened.
		return cat.RecordPeerReconciled(ctx, summary)
	}
}
