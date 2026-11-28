package worker

import (
	"context"
	"errors"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// reconcileBatch bounds one PAGE of a sweep (#942).
//
// A sweep walks every page, so the whole library is reconciled on every pass;
// the bound only keeps one page's ids, not the library's, in memory at a time.
// (It once bounded the sweep itself, with no cursor — so every pass re-read the
// same oldest page, and a want past it was never swept again.) A long sweep is
// safe: the lease heartbeats, and a sweep cut short re-derives everything on
// the next beat because each want's conclusion is committed as it is reached.
const reconcileBatch = 5000

// ReconcileStore is the part of the catalog the reconciliation sweep uses. A
// *catalog.Catalog satisfies it; the handler depends on no more than it calls.
type ReconcileStore interface {
	DesiredItemsToReconcile(ctx context.Context, after catalog.SweepCursor, limit int) ([]string, catalog.SweepCursor, error)
	ReconcileDesired(ctx context.Context, desiredItemID string) (catalog.ReconcileResult, error)
}

// ReconcileHandler answers §56's two questions for every want, or for one.
//
// # Idempotent, and silent when nothing changed
//
// It will be re-run (invariant 9). Running it twice over an unchanged library
// writes the same answers the second time, which the catalog recognises as a
// no-op and does not emit for — a sweep over a steady library on a timer would
// otherwise turn the event log into a heartbeat, and an event stream that is
// mostly noise is one nobody follows.
//
// # One bad want does not stop the sweep
//
// A want whose profile was deleted, or whose acquisition row is missing, is a
// data problem for that want. Failing the whole job would mean one broken row
// stops the entire library being reconciled, which is a much worse outcome
// than a logged error and a sweep that finishes.
func ReconcileHandler(cat ReconcileStore, log *slog.Logger) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		payload, err := decodeOptionalPayload[acquisition.ReconcilePayload](job)
		if err != nil {
			return err
		}

		// One want, or every page of the library (#942).
		var (
			cursor                 catalog.SweepCursor
			swept, changed, failed int
		)
		for {
			ids := []string{payload.DesiredItemID}
			if payload.DesiredItemID == "" {
				var err error
				ids, cursor, err = cat.DesiredItemsToReconcile(ctx, cursor, reconcileBatch)
				if err != nil {
					return err
				}
			}

			for _, id := range ids {
				// The lease is tied to this context. Stopping mid-sweep is safe
				// rather than wasteful: each want's conclusion is committed as it
				// is reached, so the next pass re-derives the rest.
				if err := ctx.Err(); err != nil {
					log.Info("reconciliation stopped early",
						"reconciled", swept-failed, "reason", err)
					return nil
				}
				swept++

				result, err := cat.ReconcileDesired(ctx, id)
				if err != nil {
					failed++
					// A want that cannot be reconciled is logged and skipped. The
					// alternative — failing the job — lets one broken row stop the
					// whole library.
					log.Warn("could not reconcile a desired item",
						"desired_item_id", id, "error", err)
					continue
				}
				if result.Changed {
					changed++
					log.Info("satisfaction changed",
						"desired_item_id", id,
						"state", result.State.Name(),
						"content", string(result.Content.Satisfaction),
						"placement", string(result.Placement.Satisfaction),
						"missing_peers", result.Placement.Missing)
				}
			}
			// A scoped run is one want; a page shorter than the batch was the
			// library's last.
			if payload.DesiredItemID != "" || len(ids) < reconcileBatch {
				break
			}
		}

		// Logged only when something happened. A sweep that changed nothing is
		// the normal case and should be invisible.
		if changed > 0 || failed > 0 {
			log.Info("reconciliation swept",
				"wants", swept, "changed", changed, "failed", failed)
		}
		if failed > 0 && failed == swept {
			// Every want failed. That is not a data problem in one row, it is
			// something systemic — a missing table, a broken profile join —
			// and it should reach the queue as a failure rather than as a
			// quiet log line.
			return errors.New("worker: every desired item failed to reconcile")
		}
		return nil
	}
}
