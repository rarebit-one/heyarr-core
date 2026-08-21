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
// promised one since Milestone 1 and this is not it: one beat, one job type,
// no cron expressions, no registry. When the upgrade scan and the search job
// need beats of their own they will show what the shape should be, and
// generalising before three users exist is how a scheduler ends up with
// features nobody asked for.

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
