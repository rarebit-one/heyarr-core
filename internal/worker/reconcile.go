package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// reconcileBatch bounds one sweep.
//
// A library with a hundred thousand wants would otherwise hold them all in one
// job's context and one lease. The bound is generous — a homelab does not have
// a hundred thousand wants — and it exists so that the failure mode of a very
// large library is "the sweep takes several passes" rather than "the sweep
// times out and nothing is ever reconciled".
const reconcileBatch = 5000

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
func ReconcileHandler(cat *catalog.Catalog, log *slog.Logger) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		var payload acquisition.ReconcilePayload
		if len(job.Payload) > 0 {
			if err := json.Unmarshal(job.Payload, &payload); err != nil {
				return fmt.Errorf("worker: reconcile_desired payload is not decodable: %w", err)
			}
		}

		ids := []string{payload.DesiredItemID}
		if payload.DesiredItemID == "" {
			var err error
			ids, err = cat.DesiredItemsToReconcile(ctx, reconcileBatch)
			if err != nil {
				return err
			}
		}

		var changed, failed int
		for _, id := range ids {
			// The lease is tied to this context. Stopping mid-sweep is safe
			// rather than wasteful: each want's conclusion is committed as it
			// is reached, so the next pass re-derives the rest.
			if err := ctx.Err(); err != nil {
				log.Info("reconciliation stopped early",
					"reconciled", len(ids)-failed, "remaining", len(ids), "reason", err)
				return nil
			}

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

		// Logged only when something happened. A sweep that changed nothing is
		// the normal case and should be invisible.
		if changed > 0 || failed > 0 {
			log.Info("reconciliation swept",
				"wants", len(ids), "changed", changed, "failed", failed)
		}
		if failed > 0 && failed == len(ids) {
			// Every want failed. That is not a data problem in one row, it is
			// something systemic — a missing table, a broken profile join —
			// and it should reach the queue as a failure rather than as a
			// quiet log line.
			return errors.New("worker: every desired item failed to reconcile")
		}
		return nil
	}
}
