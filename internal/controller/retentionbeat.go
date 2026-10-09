package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/retention"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// startRetention starts the retention beat, which prunes old events and
// finished job rows on a periodic cadence (#721).
//
// # Why the controller, and not a worker
//
// Pruning the controller DB is the same category as the backup beat: only the
// controller holds the writer for this database, so the work cannot be
// delegated to a worker without crossing invariant 4 (ADR-0002). The pruner
// issues plain SQL DELETEs through the single-writer pool, which is the same
// approach every other controller-side mutation takes.
//
// # Batching and the write lock
//
// Each beat pass prunes in small batches (retention.batchSize rows per table)
// to avoid holding the write lock for a long time. The cycle loops until all
// tables report nothing left to prune, then runs PRAGMA incremental_vacuum to
// return freed pages to the OS. Each batch is a separate statement, so a
// shutdown between batches is clean.
//
// # Zero intervals disable retention
//
// If all three retention windows are zero, the beat is not started. Each
// individual window may be zero to disable that dimension while leaving the
// others active.
func startRetention(
	ctx context.Context,
	db *sqlite.DB,
	eventLog *events.Log,
	eventsWindow, succeededWindow, deadWindow time.Duration,
	log *slog.Logger,
	newTicker tickerFunc,
) {
	if eventsWindow == 0 && succeededWindow == 0 && deadWindow == 0 {
		log.Info("retention pruning is disabled (all retention windows are 0 or empty)")
		return
	}

	pruner := retention.New(db.Writer())

	cycle := func(_ string) {
		now := time.Now().UTC()

		opts := retention.PruneOptions{}
		if eventsWindow > 0 {
			opts.EventsBefore = now.Add(-eventsWindow)
		}
		if succeededWindow > 0 {
			opts.JobsSucceededBefore = now.Add(-succeededWindow)
		}
		if deadWindow > 0 {
			opts.JobsDeadBefore = now.Add(-deadWindow)
		}

		var total retention.PruneResult
		// Loop until all tables are below the batch threshold. A node catching
		// up from years of accumulated rows may need many passes per beat.
		for {
			res, more, err := pruner.PruneOnce(ctx, opts)
			if err != nil {
				log.Warn("retention prune failed; will retry on next tick", "error", err)
				break
			}
			total.EventsDeleted += res.EventsDeleted
			total.JobsSucceededDeleted += res.JobsSucceededDeleted
			total.JobsDeadDeleted += res.JobsDeadDeleted
			if !more {
				break
			}
		}

		// Run incremental_vacuum after pruning. This is a no-op until the DB
		// has been rebuilt with auto_vacuum=INCREMENTAL (migration 00060 does
		// this on first run); on a rebuilt DB it returns freed pages to the OS.
		if err := pruner.IncrementalVacuum(ctx); err != nil {
			log.Warn("incremental_vacuum failed; the database file will not shrink this cycle", "error", err)
		}

		// Emit one per-cycle event regardless of how many rows were removed.
		// A cycle that removed nothing is still the confirmation that the beat
		// is running and everything is within the retention window (invariant 7;
		// see events.TypeRetentionCycled).
		_, emitErr := eventLog.Emit(ctx, events.TypeRetentionCycled, "system", "retention", map[string]int64{
			"events_deleted":         total.EventsDeleted,
			"jobs_succeeded_deleted": total.JobsSucceededDeleted,
			"jobs_dead_deleted":      total.JobsDeadDeleted,
		})
		if emitErr != nil {
			log.Warn("could not emit retention.cycled event", "error", emitErr)
		}

		if total.EventsDeleted+total.JobsSucceededDeleted+total.JobsDeadDeleted > 0 {
			log.Info("retention cycle complete",
				"events_deleted", total.EventsDeleted,
				"jobs_succeeded_deleted", total.JobsSucceededDeleted,
				"jobs_dead_deleted", total.JobsDeadDeleted,
			)
		}
	}

	startBeat(ctx, log, newTicker, beat{
		name:     "retention",
		interval: retentionInterval,
		startup:  true,
		attrs: []any{
			"events_window", eventsWindow,
			"jobs_succeeded_window", succeededWindow,
			"jobs_dead_window", deadWindow,
		},
		pass: cycle,
	})
}

// retentionInterval is how often the retention beat runs. An hour is frequent
// enough to keep the tables trim without adding measurable load: each pass
// only removes the rows that fell out of the window since the last run, which
// on a busy node is at most a few thousand rows an hour.
const retentionInterval = 1 * time.Hour
