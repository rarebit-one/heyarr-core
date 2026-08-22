package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The search scheduler's storage half (#130).
//
// The POLICY — which schedule a want is on, how far to back off, when it is
// next due — is pure and lives in internal/domain/acquisition/schedule.go.
// What lives here is the query that finds what is due and the write that
// records an attempt, in the same split as every other beat in this package.

// sortableTimestamp is a FIXED-WIDTH RFC 3339 layout, and the width is the
// whole reason it exists.
//
// timestampFormat is time.RFC3339Nano, which TRIMS trailing zeros from the
// fractional second: 12:00:00 UTC formats as "…T12:00:00Z" while one
// nanosecond later formats as "…T12:00:00.000000001Z". SQLite compares TEXT
// lexicographically, and 'Z' (0x5A) sorts after '.' (0x2E) — so under
// RFC3339Nano the LATER instant compares as the EARLIER string whenever the
// two fall in the same second.
//
// Everything in this file is a `next_search_at <= now` comparison against
// values that carry a sub-second spread by construction, so that collision is
// not hypothetical here. Padding the fraction to a fixed nine digits makes
// lexicographic order and chronological order the same order again. It still
// parses as RFC3339Nano, so readers do not need to know.
const sortableTimestamp = "2006-01-02T15:04:05.000000000Z07:00"

func sortable(t time.Time) string { return t.UTC().Format(sortableTimestamp) }

// DueSearch is one want the scheduler should search now.
type DueSearch struct {
	DesiredItemID string
	// Schedule is the cadence this want is on, chosen by acquisition.ScheduleFor
	// from the want's state. It is carried rather than re-derived so the caller
	// records the attempt against the same schedule it decided with.
	Schedule acquisition.Schedule
	// Fruitless is how many consecutive prior searches ON THIS SCHEDULE left
	// the want where it was — the exponent for the backoff.
	//
	// It is derived at read time rather than incremented by the search job,
	// and that is deliberate. "Did the last search change anything?" has an
	// authoritative answer already sitting in acquisition_state: a want that
	// was searched and is STILL resting on the same schedule was not moved by
	// that search. Asking the state means the scheduler cannot drift out of
	// step with reality — there is no counter for a crashed worker, a dead job
	// or a manual override to leave stale.
	Fruitless int
	// FirstEver is true when this want has never been scheduled. It exists so
	// a caller can log the difference between "starting to look for this" and
	// "still looking", which is the distinction #130 says the system could not
	// make about itself.
	FirstEver bool
}

// DueSearches lists the wants that should be searched as of now.
//
// A want with no schedule row is due immediately: a want nobody has ever
// looked for is the most urgent kind there is, and it is also what makes the
// table pure bookkeeping — losing it costs one extra search per want and
// nothing else.
//
// Ordered by how overdue each want is, so a limit truncates the least urgent
// rather than an arbitrary slice.
func (c *Catalog) DueSearches(ctx context.Context, now time.Time, limit int) ([]DueSearch, error) {
	if limit <= 0 {
		return nil, nil
	}
	// The phase filter is in the QUERY rather than in Go. Most of a library is
	// resting most of the time, but a library mid-import is not, and reading
	// every want in order to discard the ones that are downloading is the sort
	// of pass that is fine until the first time it matters.
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT d.id, d.monitor, a.phase, a.managed, a.content, a.placement,
		       coalesce(s.schedule, ''), coalesce(s.fruitless, 0),
		       coalesce(s.next_search_at, '')
		FROM desired_items d
		JOIN acquisition_state a ON a.desired_item_id = d.id
		LEFT JOIN search_schedule s ON s.desired_item_id = d.id
		WHERE a.phase = 'idle'
		  AND (s.next_search_at IS NULL OR s.next_search_at <= ?)
		ORDER BY coalesce(s.next_search_at, ''), d.id
		LIMIT ?`, sortable(now), limit)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing wants due a search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DueSearch
	for rows.Next() {
		var (
			id, phase, content, placement string
			storedSchedule, storedNext    string
			monitor, managed, fruitless   int
		)
		if err := rows.Scan(&id, &monitor, &phase, &managed, &content, &placement,
			&storedSchedule, &fruitless, &storedNext); err != nil {
			return nil, fmt.Errorf("catalog: reading wants due a search: %w", err)
		}
		state := acquisition.State{
			Phase:     acquisition.Phase(phase),
			Managed:   managed == 1,
			Content:   acquisition.Satisfaction(content),
			Placement: acquisition.Satisfaction(placement),
		}
		schedule, wanted := acquisition.ScheduleFor(state, monitor == 1)
		if !wanted {
			// Satisfied and unmonitored: finished, and not the scheduler's
			// business. Filtered here rather than in SQL because the mapping
			// from a state to a schedule is policy and lives in exactly one
			// place (see ScheduleFor); a WHERE clause that encoded it would be
			// a second place, in a language that cannot be unit-tested.
			continue
		}

		due := DueSearch{DesiredItemID: id, Schedule: schedule, FirstEver: storedNext == ""}
		if storedSchedule == schedule.Name {
			// Same question as last time, and the want has not moved: this
			// search is one further into the streak. A want that has CHANGED
			// schedule starts a fresh one, because it is now asking something
			// different and yesterday's silence says nothing about it.
			due.Fruitless = fruitless + 1
		}
		out = append(out, due)
	}
	return out, rows.Err()
}

// RecordSearchScheduled records that a search was just enqueued for a want,
// and when the next one is due.
//
// # It is a compare-and-set, and that is the point
//
// The update applies only while the stored row is still due as of this pass —
// so two schedulers running the same pass concurrently advance the row once
// between them, and the loser gets false rather than double-counting the
// streak and doubling the interval twice.
//
// That is belt to the queue's braces. The dedupe key on the search job is what
// guarantees ONE SEARCH (invariant 9, and see acquisition.SearchDedupeKey);
// this guarantees one piece of BOOKKEEPING, which is a different property and
// would otherwise silently drift on any multi-role deployment.
//
// Returns whether this caller was the one that advanced the schedule.
func (c *Catalog) RecordSearchScheduled(
	ctx context.Context, desiredItemID string, s acquisition.Schedule,
	fruitless int, now, next time.Time,
) (bool, error) {
	if desiredItemID == "" {
		return false, fmt.Errorf("catalog: recording a scheduled search needs a want")
	}
	nowStr, nextStr := sortable(now), sortable(next)

	// No event. This is bookkeeping and not a state transition (invariant 7
	// governs the latter): the transition that HAPPENED here is the job being
	// enqueued, which the queue already emits, and emitting a second event per
	// want per pass would turn the log into a heartbeat.
	res, err := c.db.Writer().ExecContext(ctx, `
		INSERT INTO search_schedule
			(desired_item_id, schedule, fruitless, last_searched_at, next_search_at,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (desired_item_id) DO UPDATE SET
			schedule         = excluded.schedule,
			fruitless        = excluded.fruitless,
			last_searched_at = excluded.last_searched_at,
			next_search_at   = excluded.next_search_at,
			updated_at       = excluded.updated_at
		WHERE search_schedule.next_search_at <= excluded.last_searched_at`,
		desiredItemID, s.Name, fruitless, nowStr, nextStr, nowStr, nowStr)
	if err != nil {
		return false, fmt.Errorf("catalog: recording a scheduled search for %s: %w", desiredItemID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("catalog: recording a scheduled search for %s: %w", desiredItemID, err)
	}
	return n > 0, nil
}

// SearchScheduleRow is one want's bookkeeping, for tests and for anything that
// needs to explain when Heyarr will next look.
type SearchScheduleRow struct {
	DesiredItemID  string
	Schedule       string
	Fruitless      int
	LastSearchedAt time.Time
	NextSearchAt   time.Time
}

// SearchSchedule reads one want's row, if it has one.
func (c *Catalog) SearchSchedule(ctx context.Context, desiredItemID string) (SearchScheduleRow, bool, error) {
	var (
		r          SearchScheduleRow
		last, next string
	)
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT desired_item_id, schedule, fruitless, last_searched_at, next_search_at
		FROM search_schedule WHERE desired_item_id = ?`, desiredItemID).
		Scan(&r.DesiredItemID, &r.Schedule, &r.Fruitless, &last, &next)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return SearchScheduleRow{}, false, nil
	case err != nil:
		return SearchScheduleRow{}, false, fmt.Errorf("catalog: reading the search schedule for %s: %w",
			desiredItemID, err)
	}
	r.LastSearchedAt, _ = time.Parse(time.RFC3339Nano, last)
	r.NextSearchAt, _ = time.Parse(time.RFC3339Nano, next)
	return r, true, nil
}

// IndexerHealth reports what the last health pass observed about the providers
// that can SEARCH, keyed by provider name.
//
// Only providers whose recorded capabilities include `indexer`: a download
// client being down says nothing about whether searching is worth attempting,
// and a scheduler that held off because Transmission was unreachable would be
// wrong in the most confusing possible way.
//
// A provider that is configured but has never been checked has no row and does
// not appear. That absence is UNKNOWN and not unhealthy, and the caller must
// keep the two apart — the same distinction §56 makes on its satisfaction
// axes, for the same reason: "nobody has looked" and "we looked and the answer
// is no" lead to different actions.
func (c *Catalog) IndexerHealth(ctx context.Context) (map[string]providers.Health, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT name, capabilities, healthy, detail, version, checked_at
		FROM provider_health`)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading indexer health: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]providers.Health{}
	for rows.Next() {
		var (
			name, caps, detail, version string
			healthy                     int
			checkedAt                   sql.NullString
		)
		if err := rows.Scan(&name, &caps, &healthy, &detail, &version, &checkedAt); err != nil {
			return nil, fmt.Errorf("catalog: reading indexer health: %w", err)
		}
		var declared []string
		if err := json.Unmarshal([]byte(caps), &declared); err != nil {
			// Corruption in one row is not a reason to refuse the whole
			// answer. Skipping it renders as "this provider is unknown", which
			// is honest about what can be told from the row.
			c.log.Warn("a provider health row has undecodable capabilities",
				"provider", name, "error", err)
			continue
		}
		if !contains(declared, string(providers.CapabilityIndexer)) {
			continue
		}
		h := providers.Health{Healthy: healthy == 1, Detail: detail, Version: version}
		if checkedAt.Valid {
			if t, err := time.Parse(timestampFormat, checkedAt.String); err == nil {
				h.CheckedAt = t
			}
		}
		out[name] = h
	}
	return out, rows.Err()
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
