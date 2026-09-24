package catalog

import (
	"context"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// The fruitless-backoff bookkeeping shared by the search, subtitle fetch and
// enrich schedules.
//
// Each of those tables has the same shape: one row per subject (a want, or a
// Work), a fruitless streak that is the backoff exponent, when the subject was
// last attempted and when it is next due. The POLICY — how far to back off — is
// the caller's; what lives here is the SQL that every one of them repeats: the
// LEFT JOIN that makes a subject with no row due immediately, the "is it due"
// predicate, the most-overdue-first ordering, and the upsert that records an
// attempt.
//
// Every timestamp is written with sqlite.FormatTimestamp so that SQLite's
// lexicographic TEXT order is the chronological order the due comparison and the
// ordering depend on (see sqlite.TimestampLayout).

// backoffSchedule names one schedule table and its columns. The fields are only
// ever the package-level literals below, never input, which is what makes the
// string-built SQL in the methods safe.
type backoffSchedule struct {
	table string // the schedule table
	key   string // the subject column, and the table's conflict target
	last  string // when the subject was last attempted
	next  string // when the subject is next due
}

var (
	searchBackoff = backoffSchedule{
		table: "search_schedule", key: "desired_item_id",
		last: "last_searched_at", next: "next_search_at",
	}
	subtitleFetchBackoff = backoffSchedule{
		table: "subtitle_fetch_schedule", key: "desired_item_id",
		last: "last_fetched_at", next: "next_fetch_at",
	}
	enrichBackoff = backoffSchedule{
		table: "enrich_schedule", key: "work_id",
		last: "last_enriched_at", next: "next_enrich_at",
	}
)

// leftJoin joins the schedule as `s` onto subject (a column expression such as
// "d.id"). It is a LEFT JOIN because a subject with no row has never been
// attempted, and that is the most urgent kind: dueWhere treats it as due.
func (b backoffSchedule) leftJoin(subject string) string {
	return `LEFT JOIN ` + b.table + ` s ON s.` + b.key + ` = ` + subject
}

// dueWhere is the "due as of ?" predicate over the joined `s`. The caller binds
// sqlite.FormatTimestamp(now) to its placeholder.
func (b backoffSchedule) dueWhere() string {
	return `(s.` + b.next + ` IS NULL OR s.` + b.next + ` <= ?)`
}

// dueOrder orders by how overdue each subject is, never-attempted first, so a
// limit truncates the least urgent rather than an arbitrary slice.
func (b backoffSchedule) dueOrder() string {
	return `coalesce(s.` + b.next + `, '')`
}

// backoffColumn is a schedule-specific column an attempt also writes (the
// search schedule's schedule name).
type backoffColumn struct {
	name  string
	value any
}

// backoffAttempt is one recorded attempt.
type backoffAttempt struct {
	subject   string
	fruitless int
	now, next time.Time
	extra     []backoffColumn
	// onlyIfDue makes the update a compare-and-set: it applies only while the
	// stored row is still due as of now, so two passes racing the same subject
	// advance it once between them. A first insert always applies.
	onlyIfDue bool
}

// record upserts a subject's row: its streak, when it was attempted and when it
// is next due. It reports whether a row was written, which is only ever false
// for an onlyIfDue attempt that lost the race.
//
// No event. This is bookkeeping and not a state transition (invariant 7 governs
// the latter): the transition is the job being enqueued, which the queue already
// emits, and a second event per subject per pass would turn the log into a
// heartbeat.
func (b backoffSchedule) record(ctx context.Context, db *sqlite.DB, a backoffAttempt) (bool, error) {
	nowStr, nextStr := sqlite.FormatTimestamp(a.now), sqlite.FormatTimestamp(a.next)

	cols := b.key
	marks := `?`
	set := ``
	args := []any{a.subject}
	for _, x := range a.extra {
		cols += `, ` + x.name
		marks += `, ?`
		set += x.name + ` = excluded.` + x.name + `, `
		args = append(args, x.value)
	}
	cols += `, fruitless, ` + b.last + `, ` + b.next + `, created_at, updated_at`
	marks += `, ?, ?, ?, ?, ?`
	set += `fruitless = excluded.fruitless, ` +
		b.last + ` = excluded.` + b.last + `, ` +
		b.next + ` = excluded.` + b.next + `, ` +
		`updated_at = excluded.updated_at`
	args = append(args, a.fruitless, nowStr, nextStr, nowStr, nowStr)

	guard := ``
	if a.onlyIfDue {
		guard = ` WHERE ` + b.table + `.` + b.next + ` <= excluded.` + b.last
	}

	//nolint:gosec // every identifier is a backoffSchedule/backoffColumn literal from this package; every value is bound
	res, err := db.Writer().ExecContext(ctx, `
		INSERT INTO `+b.table+` (`+cols+`)
		VALUES (`+marks+`)
		ON CONFLICT (`+b.key+`) DO UPDATE SET `+set+guard, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// clear deletes a subject's row, so it is due as of now from a clean streak.
// Idempotent: deleting an absent row is a no-op.
func (b backoffSchedule) clear(ctx context.Context, db *sqlite.DB, subject string) error {
	//nolint:gosec // table and key are backoffSchedule literals from this package; the subject is bound
	_, err := db.Writer().ExecContext(ctx, `DELETE FROM `+b.table+` WHERE `+b.key+` = ?`, subject)
	return err
}
