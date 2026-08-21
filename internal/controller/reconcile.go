package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
)

// The reconciliation beat (§57, M3-05).
//
// # Why the controller owns it
//
// Deciding that work should happen is a control-plane decision (§7), and doing
// it is a worker's (§9). So the controller enqueues and the worker runs, which
// is also the only way the two are allowed to talk (invariant 4, ADR-0002) —
// the worker may not even be the same process.
//
// # Why a timer at all
//
// A want's satisfaction can change without the want being touched: a quality
// profile edit can unsatisfy a want nothing else went near, an asset can be
// deleted, a peer can go away (§57). Ingest hooks and API callbacks cannot see
// any of those, so something has to look periodically. This is the smallest
// thing that does.
//
// It is deliberately NOT a general scheduler. The controller's package doc has
// promised one since Milestone 1 and this is not it: two beats, two job types,
// no cron expressions, no registry.
//
// M3-05 wrote that generalising before three users exist is how a scheduler
// grows features nobody asked for. M3-06 is the second user and it did not
// change that judgement — the second beat is nine lines and differs from the
// first only in its interval and its job type. When the search job (M3-12)
// arrives as the third, the shape will be obvious from three examples rather
// than guessed from one.

// reconcileInterval is how often the whole library is re-examined.
//
// Five minutes is chosen against what it costs and what it catches. It costs
// one indexed pass over the wants plus a handful of joins each — nothing on a
// homelab library — and it is short enough that "I edited the profile and
// Heyarr still says it is satisfied" resolves before anyone opens an issue
// about it.
//
// It is a constant rather than configuration because nothing yet suggests an
// operator would want a different number, and a knob that exists only because
// it was easy to add is a knob somebody eventually sets to something harmful.
const reconcileInterval = 5 * time.Minute

// upgradeScanInterval is how often the library is examined for wants that
// could be improved (§60, M3-06).
//
// Six hours, against reconciliation's five minutes, and the gap is the point.
// The two questions have very different costs and very different urgencies:
//
//   - "is this still satisfied" is a read, and getting it wrong means an
//     operator sees a stale answer on a status page.
//   - "could this be better" leads to a provider round trip and then to moving
//     gigabytes, and getting it wrong means bandwidth.
//
// Nobody needs the second considered every five minutes. A release that
// appears at noon being noticed by evening is not a degraded experience, it is
// the normal one — and an upgrade scan on a tight loop is how a homelab
// discovers its transfer cap.
const upgradeScanInterval = 6 * time.Hour

// startReconciliation enqueues a sweep now and then on the beat.
//
// The immediate one matters: a Heyarr that has just started may have missed
// hours of change, and waiting a full interval to notice would make a restart
// look like a period of blindness.
func startReconciliation(ctx context.Context, queue *jobs.Queue, log *slog.Logger) {
	enqueue := func(reason string) {
		if err := enqueueReconcile(ctx, queue, ""); err != nil {
			// Never fatal. Reconciliation is how Heyarr notices things, not
			// how it works — a node that cannot enqueue a sweep still serves,
			// still plays and still ingests, and the next beat will try again.
			log.Warn("could not enqueue a reconciliation sweep",
				"reason", reason, "error", err)
		}
	}
	enqueue("startup")

	go func() {
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				enqueue("beat")
			}
		}
	}()
	log.Info("reconciliation beat started", "interval", reconcileInterval)
}

// startUpgradeScan enqueues an upgrade sweep on its own, much slower beat
// (§60, M3-06).
//
// Deliberately NOT enqueued at startup, unlike reconciliation. A restart is a
// reason to re-check what is satisfied — the library may have changed while
// Heyarr was not looking — but it is not a reason to go looking for better
// copies of things that are already fine. A node that restarts six times
// during a debugging session should not launch six upgrade sweeps.
func startUpgradeScan(ctx context.Context, queue *jobs.Queue, log *slog.Logger) {
	go func() {
		ticker := time.NewTicker(upgradeScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := enqueueUpgradeScan(ctx, queue, ""); err != nil {
					// Never fatal, for the same reason reconciliation is not:
					// this is how Heyarr notices things, not how it works.
					log.Warn("could not enqueue an upgrade scan", "error", err)
				}
			}
		}
	}()
	log.Info("upgrade scan beat started", "interval", upgradeScanInterval)
}

// enqueueUpgradeScan queues a sweep, or one want's scan.
func enqueueUpgradeScan(ctx context.Context, queue *jobs.Queue, desiredItemID string) error {
	dedupe := acquisition.UpgradeScanDedupeKey
	if desiredItemID != "" {
		dedupe = acquisition.UpgradeScanDedupeKey + ":" + desiredItemID
	}
	_, err := queue.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      acquisition.UpgradeScanJobType,
		Payload:   acquisition.UpgradeScanPayload{DesiredItemID: desiredItemID},
		DedupeKey: dedupe,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("controller: enqueueing an upgrade scan: %w", err)
	}
	return nil
}

// enqueueReconcile queues a sweep, or one want's reconciliation.
//
// The dedupe key is what makes the beat safe. A sweep already queued or
// running is the same sweep, so a slow pass on a large library cannot pile up
// behind itself — which is the failure mode a naive timer produces, and it
// produces it precisely when the system is already struggling.
func enqueueReconcile(ctx context.Context, queue *jobs.Queue, desiredItemID string) error {
	dedupe := acquisition.ReconcileDedupeKey
	if desiredItemID != "" {
		// A scoped reconciliation dedupes per want, so wanting five things at
		// once queues five quick jobs rather than collapsing into one.
		dedupe = acquisition.ReconcileDedupeKey + ":" + desiredItemID
	}
	_, err := queue.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      acquisition.ReconcileJobType,
		Payload:   acquisition.ReconcilePayload{DesiredItemID: desiredItemID},
		DedupeKey: dedupe,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("controller: enqueueing reconciliation: %w", err)
	}
	return nil
}
