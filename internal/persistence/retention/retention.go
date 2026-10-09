// Package retention prunes old rows from the events and jobs tables (#721).
//
// # Why these tables grow forever without retention
//
// The events table records every state transition the controller emits. On a
// production node this is dominated by job.enqueued, job.succeeded and
// job.failed — about 90 % of all rows — and on a busy node accumulates
// millions of rows per week. The jobs table keeps every finished row forever:
// succeeded and dead jobs are never re-run (invariant 9), they provide no
// correctness guarantee once finished, and without pruning they grow without
// bound.
//
// # Safety
//
// Events: the only consumer of old events is the SSE catch-up path
// (events.Log.Since), which already sends heyarr.stream.gap when a client
// reconnects behind the retention window. Backup generation uses only
// max(seq), and cross-site convergence uses catalog_ops, not events.
//
// Jobs: jobs_dedupe is a partial index over pending and leased rows only, so
// removing a finished job never breaks deduplication. No foreign keys or
// triggers reference the jobs table.
//
// # Batched deletes
//
// Deleting millions of rows in one statement would hold the write lock for
// seconds and block every other writer. Instead each call prunes at most
// batchSize rows and the beat calls it in a loop until nothing is left. Each
// small delete is also friendlier to SQLite's WAL: large deletes produce a
// large WAL frame that must checkpoint before the log file shrinks.
//
// # auto_vacuum and incremental_vacuum
//
// The database runs auto_vacuum=NONE by default. Migration 00060 switches it
// to INCREMENTAL (which requires a one-time VACUUM to rebuild the file with
// the freelist tracking pages). After each prune cycle the beat calls
// PRAGMA incremental_vacuum to return free pages to the OS, so deletes
// actually shrink the file over time. Before the one-time VACUUM, the
// incremental_vacuum call is a documented no-op.
package retention

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// batchSize is how many rows are deleted per statement. Small enough that each
// delete finishes in well under a millisecond and does not starve other
// writers, large enough that a ten-million-row backlog drains in a few
// minutes.
const batchSize = 500

// Pruner holds the single-writer connection used for all deletes.
type Pruner struct {
	db *sql.DB
}

// New returns a Pruner that writes through db (the controller's single-writer
// pool).
func New(db *sql.DB) *Pruner {
	return &Pruner{db: db}
}

// PruneResult summarises one prune cycle.
type PruneResult struct {
	// EventsDeleted is the number of events rows removed in this call.
	EventsDeleted int64
	// JobsSucceededDeleted is the number of succeeded job rows removed.
	JobsSucceededDeleted int64
	// JobsDeadDeleted is the number of dead job rows removed.
	JobsDeadDeleted int64
}

// PruneOptions controls which rows are eligible for deletion. A zero duration
// disables that dimension.
type PruneOptions struct {
	// EventsBefore deletes events with created_at before this cutoff.
	// Zero disables event pruning.
	EventsBefore time.Time
	// JobsSucceededBefore deletes succeeded jobs with finished_at before this
	// cutoff. Zero disables pruning of succeeded jobs.
	JobsSucceededBefore time.Time
	// JobsDeadBefore deletes dead jobs with finished_at before this cutoff.
	// Zero disables pruning of dead jobs.
	JobsDeadBefore time.Time
}

// PruneOnce deletes at most one batch of rows for each non-zero cutoff in
// opts. It returns the counts and whether any table may have more rows to
// prune (i.e. whether the batch was full, suggesting a second call is
// warranted).
//
// Each delete operates through the single-writer pool to preserve
// single-writer ordering (ADR-0003); no transaction spans more than one
// table's delete.
func (p *Pruner) PruneOnce(ctx context.Context, opts PruneOptions) (PruneResult, bool, error) {
	var res PruneResult
	more := false

	if !opts.EventsBefore.IsZero() {
		n, err := p.deleteEvents(ctx, opts.EventsBefore)
		if err != nil {
			return res, false, fmt.Errorf("retention: pruning events: %w", err)
		}
		res.EventsDeleted = n
		if n >= batchSize {
			more = true
		}
	}

	if !opts.JobsSucceededBefore.IsZero() {
		n, err := p.deleteJobs(ctx, "succeeded", opts.JobsSucceededBefore)
		if err != nil {
			return res, false, fmt.Errorf("retention: pruning succeeded jobs: %w", err)
		}
		res.JobsSucceededDeleted = n
		if n >= batchSize {
			more = true
		}
	}

	if !opts.JobsDeadBefore.IsZero() {
		n, err := p.deleteJobs(ctx, "dead", opts.JobsDeadBefore)
		if err != nil {
			return res, false, fmt.Errorf("retention: pruning dead jobs: %w", err)
		}
		res.JobsDeadDeleted = n
		if n >= batchSize {
			more = true
		}
	}

	return res, more, nil
}

// deleteEvents removes at most batchSize events rows older than cutoff.
func (p *Pruner) deleteEvents(ctx context.Context, cutoff time.Time) (int64, error) {
	// The subquery selects the seq values to delete — a bounded inner scan on
	// the primary key with a time filter — and the outer DELETE targets them.
	// This is SQLite's recommended pattern for a limit on a DELETE that
	// references no other table (sqlite.org/lang_delete.html).
	res, err := p.db.ExecContext(ctx, `
		DELETE FROM events WHERE seq IN (
			SELECT seq FROM events
			WHERE created_at < ?
			ORDER BY seq ASC
			LIMIT ?
		)`, cutoff.UTC().Format(time.RFC3339Nano), batchSize) // events store RFC 3339 Nano (events.timeFormat)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// deleteJobs removes at most batchSize finished job rows of the given state
// whose finished_at is before cutoff. The cutoff is written in the same
// fixed-width layout the job queue stores (sqlite.FormatTimestamp, #621), so the
// TEXT comparison is a time comparison. Live jobs (pending, leased) are never
// touched: the WHERE clause requires a terminal state, and jobs that have not
// finished have finished_at NULL, which is excluded by the < comparison.
func (p *Pruner) deleteJobs(ctx context.Context, state string, cutoff time.Time) (int64, error) {
	res, err := p.db.ExecContext(ctx, `
		DELETE FROM jobs WHERE id IN (
			SELECT id FROM jobs
			WHERE state = ? AND finished_at < ?
			ORDER BY finished_at ASC
			LIMIT ?
		)`, state, sqlite.FormatTimestamp(cutoff), batchSize)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// IncrementalVacuum runs PRAGMA incremental_vacuum to return free pages to the
// OS after a prune. It is a no-op when the database has not been rebuilt with
// auto_vacuum=INCREMENTAL (see migration 00060 and the package doc).
func (p *Pruner) IncrementalVacuum(ctx context.Context) error {
	_, err := p.db.ExecContext(ctx, `PRAGMA incremental_vacuum`)
	if err != nil {
		return fmt.Errorf("retention: incremental_vacuum: %w", err)
	}
	return nil
}
