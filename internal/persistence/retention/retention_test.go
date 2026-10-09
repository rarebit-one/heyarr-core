package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rarebit-one/heyarr-core/internal/persistence/retention"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// newPruner returns a Pruner backed by a fresh migrated DB.
func newPruner(t *testing.T) (*retention.Pruner, *sqlite.DB) {
	t.Helper()
	db := testdb.Migrated(t)
	return retention.New(db.Writer()), db
}

// insertEventAt inserts one event row with the given timestamp into the events
// table directly, bypassing events.Log to avoid test-clock complications.
func insertEventAt(t *testing.T, db *sqlite.DB, ts time.Time) int64 {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	res, err := db.Writer().ExecContext(context.Background(),
		`INSERT INTO events (id, type, created_at) VALUES (?, 'test.event', ?)`,
		id, ts.UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("insertEventAt: %v", err)
	}
	seq, _ := res.LastInsertId()
	return seq
}

// insertJobAt inserts one job row in the given state with the given finished_at
// timestamp. For non-terminal states (pending, leased), pass a zero time.
func insertJobAt(t *testing.T, db *sqlite.DB, state string, finishedAt time.Time) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var finAt interface{}
	if !finishedAt.IsZero() {
		finAt = finishedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := db.Writer().ExecContext(context.Background(),
		`INSERT INTO jobs (id, type, state, run_after, created_at, updated_at, finished_at)
		 VALUES (?, 'test.job', ?, ?, ?, ?, ?)`,
		id, state, now, now, now, finAt)
	if err != nil {
		t.Fatalf("insertJobAt(state=%s): %v", state, err)
	}
	return id
}

// countEvents returns the total number of rows in the events table.
func countEvents(t *testing.T, db *sqlite.DB) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(context.Background(), `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("countEvents: %v", err)
	}
	return n
}

// countJobs returns the number of rows with the given state.
func countJobs(t *testing.T, db *sqlite.DB, state string) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(context.Background(),
		`SELECT count(*) FROM jobs WHERE state = ?`, state).Scan(&n); err != nil {
		t.Fatalf("countJobs(%s): %v", state, err)
	}
	return n
}

// TestPruneOnce_EventsBoundary verifies that events strictly before the
// cutoff are removed, and events at or after the cutoff are kept.
func TestPruneOnce_EventsBoundary(t *testing.T) {
	t.Parallel()
	pruner, db := newPruner(t)
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)

	// One event strictly before cutoff → should be pruned.
	insertEventAt(t, db, cutoff.Add(-time.Second))
	// One event exactly at cutoff → kept (not strictly less than cutoff).
	insertEventAt(t, db, cutoff)
	// One event after cutoff → kept.
	insertEventAt(t, db, cutoff.Add(time.Second))

	if got := countEvents(t, db); got != 3 {
		t.Fatalf("precondition: expected 3 events, got %d", got)
	}

	opts := retention.PruneOptions{EventsBefore: cutoff}
	res, more, err := pruner.PruneOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if more {
		t.Error("more=true for 3 events below batchSize")
	}
	if res.EventsDeleted != 1 {
		t.Errorf("EventsDeleted = %d, want 1 (only the strictly-before event)", res.EventsDeleted)
	}
	if got := countEvents(t, db); got != 2 {
		t.Errorf("events remaining = %d, want 2", got)
	}
}

// TestPruneOnce_JobsBoundary verifies that finished jobs older than the cutoff
// are pruned and that live jobs (pending, leased) are never touched.
func TestPruneOnce_JobsBoundary(t *testing.T) {
	t.Parallel()
	pruner, db := newPruner(t)
	cutoff := time.Now().UTC().Add(-3 * 24 * time.Hour)

	// Succeeded: old (pruned), recent (kept).
	insertJobAt(t, db, "succeeded", cutoff.Add(-time.Hour))
	insertJobAt(t, db, "succeeded", cutoff.Add(time.Hour))

	// Dead: old (pruned), recent (kept).
	insertJobAt(t, db, "dead", cutoff.Add(-time.Hour))
	insertJobAt(t, db, "dead", cutoff.Add(time.Hour))

	// A pending job with no finished_at — must NEVER be pruned.
	pendingID := insertJobAt(t, db, "pending", time.Time{})

	opts := retention.PruneOptions{
		JobsSucceededBefore: cutoff,
		JobsDeadBefore:      cutoff,
	}
	res, _, err := pruner.PruneOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if res.JobsSucceededDeleted != 1 {
		t.Errorf("JobsSucceededDeleted = %d, want 1", res.JobsSucceededDeleted)
	}
	if res.JobsDeadDeleted != 1 {
		t.Errorf("JobsDeadDeleted = %d, want 1", res.JobsDeadDeleted)
	}
	if got := countJobs(t, db, "succeeded"); got != 1 {
		t.Errorf("succeeded remaining = %d, want 1", got)
	}
	if got := countJobs(t, db, "dead"); got != 1 {
		t.Errorf("dead remaining = %d, want 1", got)
	}

	// Pending job must survive untouched.
	var stillPending int
	err = db.Reader().QueryRowContext(context.Background(),
		`SELECT count(*) FROM jobs WHERE id = ? AND state = 'pending'`, pendingID).Scan(&stillPending)
	if err != nil {
		t.Fatalf("checking pending job: %v", err)
	}
	if stillPending != 1 {
		t.Error("pending job was removed; live jobs must never be pruned")
	}
}

// TestPruneOnce_ZeroWindowIsDisabled confirms that a zero PruneOptions
// removes nothing, even when old rows exist.
func TestPruneOnce_ZeroWindowIsDisabled(t *testing.T) {
	t.Parallel()
	pruner, db := newPruner(t)

	old := time.Now().Add(-365 * 24 * time.Hour)
	insertEventAt(t, db, old)
	insertJobAt(t, db, "succeeded", old)
	insertJobAt(t, db, "dead", old)

	res, _, err := pruner.PruneOnce(context.Background(), retention.PruneOptions{})
	if err != nil {
		t.Fatalf("PruneOnce with zero options: %v", err)
	}
	total := res.EventsDeleted + res.JobsSucceededDeleted + res.JobsDeadDeleted
	if total != 0 {
		t.Errorf("zero PruneOptions removed %d rows; it must remove nothing", total)
	}
}

// TestPruneOnce_MoreFlag verifies that more=true is returned when a table has
// more rows than one batch to prune, and that repeated calls eventually drain
// all eligible rows.
func TestPruneOnce_MoreFlag(t *testing.T) {
	t.Parallel()
	// Insert 501 rows. The batchSize is 500, so the first call removes 500 and
	// reports more=true; the second removes 1 and reports more=false.
	const rows = 501
	pruner, db := newPruner(t)
	old := time.Now().Add(-365 * 24 * time.Hour)
	for range rows {
		insertEventAt(t, db, old)
	}

	opts := retention.PruneOptions{EventsBefore: time.Now()}

	res1, more1, err := pruner.PruneOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("first PruneOnce: %v", err)
	}
	if res1.EventsDeleted == 0 {
		t.Fatal("first PruneOnce removed nothing")
	}
	if !more1 {
		t.Errorf("more=false after first batch; expected more with %d rows", rows)
	}

	res2, more2, err := pruner.PruneOnce(context.Background(), opts)
	if err != nil {
		t.Fatalf("second PruneOnce: %v", err)
	}
	if more2 {
		t.Errorf("more=true after draining remaining rows")
	}
	if got := res1.EventsDeleted + res2.EventsDeleted; got != rows {
		t.Errorf("total deleted = %d, want %d", got, rows)
	}
}

// TestIncrementalVacuum verifies that IncrementalVacuum runs without error.
// It does not assert file-size changes because that depends on the database
// having been built with auto_vacuum=INCREMENTAL (which requires a VACUUM after
// migration 00060); on a fresh test DB the call is a no-op, and that is OK.
func TestIncrementalVacuum(t *testing.T) {
	t.Parallel()
	pruner, _ := newPruner(t)
	if err := pruner.IncrementalVacuum(context.Background()); err != nil {
		t.Errorf("IncrementalVacuum: %v", err)
	}
}
