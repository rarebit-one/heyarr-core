package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/desired"
	"github.com/rarebit-one/heyarr-core/internal/domain/strategy"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The search scheduler's storage half (#130).
//
// The POLICY — which schedule a want is on, how far to back off, when it is
// next due — is pure and lives in internal/domain/acquisition/schedule.go.
// What lives here is the query that finds what is due and the write that
// records an attempt, in the same split as every other beat in this package.

// The table's due predicate, ordering and upsert are the shared fruitless-backoff
// bookkeeping in backoffschedule.go (searchBackoff).

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
	//nolint:gosec // the concatenated fragments are searchBackoff's literal table and columns; now is bound
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT d.id, d.monitor, a.phase, a.managed, a.content, a.placement,
		       coalesce(w.content_type, ''), d.aspect,
		       coalesce(s.schedule, ''), coalesce(s.fruitless, 0),
		       `+searchBackoff.dueOrder()+`
		FROM desired_items d
		JOIN acquisition_state a ON a.desired_item_id = d.id
		JOIN works w ON w.id = d.work_id
		`+searchBackoff.leftJoin("d.id")+`
		WHERE a.phase = 'idle'
		  AND `+searchBackoff.dueWhere()+`
		ORDER BY `+searchBackoff.dueOrder()+`, d.id`, sqlite.FormatTimestamp(now))
	if err != nil {
		return nil, fmt.Errorf("catalog: listing wants due a search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DueSearch
	for rows.Next() {
		var (
			id, phase, content, placement string
			contentType, aspect           string
			storedSchedule, storedNext    string
			monitor, managed, fruitless   int
		)
		if err := rows.Scan(&id, &monitor, &phase, &managed, &content, &placement,
			&contentType, &aspect, &storedSchedule, &fruitless, &storedNext); err != nil {
			return nil, fmt.Errorf("catalog: reading wants due a search: %w", err)
		}
		state := acquisition.State{
			Phase:     acquisition.Phase(phase),
			Managed:   managed == 1,
			Content:   acquisition.Satisfaction(content),
			Placement: acquisition.Satisfaction(placement),
		}
		// The route is the want's content-type strategy (ADR-0082): a direct-route
		// want — a document, a podcast — is taken from its feed and never asked of
		// an indexer, so ScheduleFor refuses it whatever its state.
		route := strategy.For(contentType).Route
		// A subtitle want is direct whatever the video's content type (ADR-0085):
		// its bytes come from a subtitle provider, never a torznab indexer, so the
		// aspect overrides the content-type route before the schedule is decided.
		if aspect == string(desired.AspectSubtitle) {
			route = acquisition.RouteDirect
		}
		schedule, wanted := acquisition.ScheduleFor(state, monitor == 1, route)
		if !wanted {
			// Not on any search schedule: a direct-route want, or one that is
			// satisfied and unmonitored — finished, and not the scheduler's
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
		// Limit eligible searches, not candidate rows. A leading batch of
		// direct-route or finished wants otherwise hides later wants forever.
		// Stream candidates so the result remains bounded without duplicating
		// ScheduleFor's policy in SQL.
		if len(out) == limit {
			break
		}
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
	// The CAS guard is onlyIfDue; no event, for the reason record gives.
	advanced, err := searchBackoff.record(ctx, c.db, backoffAttempt{
		subject:   desiredItemID,
		fruitless: fruitless,
		now:       now,
		next:      next,
		extra:     []backoffColumn{{name: "schedule", value: s.Name}},
		onlyIfDue: true,
	})
	if err != nil {
		return false, fmt.Errorf("catalog: recording a scheduled search for %s: %w", desiredItemID, err)
	}
	return advanced, nil
}

// ClearSearchSchedule removes a want's search bookkeeping, so it is due a search
// as of now.
//
// DueSearches LEFT JOINs the schedule and treats a missing row as due
// immediately (a want nobody has looked for is the most urgent kind). Deleting
// the row is therefore how a caller says "look again now, from a clean streak"
// without reaching into next_search_at — used when a want has been re-driven out
// of band (its failed release blocked) and must not sit out the backoff the
// failed search left behind. Idempotent: deleting an absent row is a no-op.
func (c *Catalog) ClearSearchSchedule(ctx context.Context, desiredItemID string) error {
	if desiredItemID == "" {
		return fmt.Errorf("catalog: clearing a search schedule needs a want")
	}
	if err := searchBackoff.clear(ctx, c.db, desiredItemID); err != nil {
		return fmt.Errorf("catalog: clearing the search schedule for %s: %w", desiredItemID, err)
	}
	return nil
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
	r.LastSearchedAt, _ = sqlite.ParseTimestamp(last)
	r.NextSearchAt, _ = sqlite.ParseTimestamp(next)
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
