package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The enrich schedule (ADR-0087): which held music/book Works are due an enrich
// pass, and the bookkeeping that paces the keyless providers. The sibling of the
// subtitle fetch schedule (subtitlefetch.go) for a job over a Work rather than a
// want.

// DueEnrichWork is one held Work the enrich beat should ask a provider about now.
// It carries what the enrich job needs to build an EnrichQuery without re-reading
// the Work.
type DueEnrichWork struct {
	WorkID      string
	ContentType string // "music" | "book"
	Title       string
	Year        int
	// Artist and Author are the identity attributes ingest parsed, read from the
	// Work's attributes column. Only the one matching the content type is set in
	// practice; both are returned so the worker builds the query without a second
	// read.
	Artist string
	Author string
	// Fruitless is how many consecutive prior passes did not fully enrich the Work
	// — the backoff exponent.
	Fruitless int
}

// enrichDueWhere is the shared predicate for "a held music/book Work that is
// under-enriched": it is HELD (a managed asset with bytes exists on one of its
// editions — a wanted-but-unacquired Work is not enriched), and it lacks EITHER a
// resolvable cover OR an enrich external id (ADR-0087's "no artwork and/or no
// enrich id"). A Work with both is fully enriched and drops out; one missing
// either stays eligible, paced by the schedule's backoff.
const enrichDueWhere = `
	w.content_type IN ('music','book')
	AND EXISTS (
		SELECT 1 FROM editions e JOIN assets a ON a.edition_id = e.id
		WHERE e.work_id = w.id AND a.source_class = 'managed'
		  AND a.blob_hash IS NOT NULL AND a.missing_since IS NULL
	)
	AND (
		NOT EXISTS (
			SELECT 1 FROM editions e2 JOIN assets aw ON aw.edition_id = e2.id
			WHERE e2.work_id = w.id AND aw.role = 'artwork'
			  AND aw.blob_hash IS NOT NULL AND aw.missing_since IS NULL
		)
		OR NOT EXISTS (
			SELECT 1 FROM external_ids x
			WHERE x.entity_type = 'work' AND x.entity_id = w.id
			  AND x.source IN ('musicbrainz','openlibrary')
		)
	)`

// DueEnrichWorks lists the held music/book Works due an enrich pass as of now.
// A Work qualifies per enrichDueWhere and when its schedule says it is due (or it
// has never been enriched). Ordered by how overdue each is, so a limit truncates
// the least urgent.
func (c *Catalog) DueEnrichWorks(ctx context.Context, now time.Time, limit int) ([]DueEnrichWork, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT w.id, w.content_type, w.title, coalesce(w.year, 0),
		       coalesce(json_extract(w.attributes, '$.artist'), ''),
		       coalesce(json_extract(w.attributes, '$.author'), ''),
		       coalesce(s.fruitless, 0)
		FROM works w
		LEFT JOIN enrich_schedule s ON s.work_id = w.id
		WHERE `+enrichDueWhere+`
		  AND (s.next_enrich_at IS NULL OR s.next_enrich_at <= ?)
		ORDER BY coalesce(s.next_enrich_at, ''), w.id
		LIMIT ?`, sortable(now), limit)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing works due enrichment: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DueEnrichWork
	for rows.Next() {
		var d DueEnrichWork
		if err := rows.Scan(&d.WorkID, &d.ContentType, &d.Title, &d.Year,
			&d.Artist, &d.Author, &d.Fruitless); err != nil {
			return nil, fmt.Errorf("catalog: reading works due enrichment: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// EnrichContext reads one Work's enrich context, re-read at handling time so
// nothing carried in the payload goes stale. ok is false when the Work is gone or
// is no longer a music/book Work — in which case the job is a no-op. It does NOT
// re-check the under-enriched predicate: a Work enriched between enqueue and
// handling simply writes ids/cover that already exist (idempotent) and reschedules
// far out, which is cheaper than a second correlated read here.
func (c *Catalog) EnrichContext(ctx context.Context, workID string) (DueEnrichWork, bool, error) {
	var d DueEnrichWork
	d.WorkID = workID
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT w.content_type, w.title, coalesce(w.year, 0),
		       coalesce(json_extract(w.attributes, '$.artist'), ''),
		       coalesce(json_extract(w.attributes, '$.author'), ''),
		       coalesce(s.fruitless, 0)
		FROM works w
		LEFT JOIN enrich_schedule s ON s.work_id = w.id
		WHERE w.id = ? AND w.content_type IN ('music','book')`, workID).
		Scan(&d.ContentType, &d.Title, &d.Year, &d.Artist, &d.Author, &d.Fruitless)
	if errors.Is(err, sql.ErrNoRows) {
		return DueEnrichWork{}, false, nil
	}
	if err != nil {
		return DueEnrichWork{}, false, fmt.Errorf("catalog: reading enrich context for %s: %w", workID, err)
	}
	return d, true, nil
}

// RecordEnrichScheduled records that a Work was enriched and when it is due again
// — the enrich beat's bookkeeping, the sibling of RecordSubtitleFetchScheduled. No
// event: enqueuing the job is the transition the queue already emits, and a second
// event per Work per pass would turn the log into a heartbeat.
func (c *Catalog) RecordEnrichScheduled(
	ctx context.Context, workID string, fruitless int, now, next time.Time,
) error {
	if workID == "" {
		return fmt.Errorf("catalog: recording a scheduled enrich needs a work")
	}
	nowStr, nextStr := sortable(now), sortable(next)
	_, err := c.db.Writer().ExecContext(ctx, `
		INSERT INTO enrich_schedule
			(work_id, fruitless, last_enriched_at, next_enrich_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (work_id) DO UPDATE SET
			fruitless        = excluded.fruitless,
			last_enriched_at = excluded.last_enriched_at,
			next_enrich_at   = excluded.next_enrich_at,
			updated_at       = excluded.updated_at`,
		workID, fruitless, nowStr, nextStr, nowStr, nowStr)
	if err != nil {
		return fmt.Errorf("catalog: recording a scheduled enrich: %w", err)
	}
	return nil
}

// EnrichScope narrows a backfill to part of the library. An empty field is "no
// filter on that dimension"; the caller has already refused a wholly-unscoped
// request unless All is set.
type EnrichScope struct {
	LibraryID string
	WorkID    string
	// Author matches the Work's attributes.author (or attributes.artist) exactly —
	// the shelf-name grouping list_authors/list_artists show, so an operator can
	// enrich "everything filed under Reads" in one call.
	Author string
	All    bool
}

// WorksNeedingEnrich returns the ids of held music/book Works that are
// under-enriched (per enrichDueWhere) within a scope, IGNORING the backoff
// schedule — the "do it now" set an operator backfill enqueues, distinct from
// DueEnrichWorks which the beat paces. Ordered by title for a stable, readable
// batch.
func (c *Catalog) WorksNeedingEnrich(ctx context.Context, scope EnrichScope) ([]string, error) {
	where := enrichDueWhere
	var args []any
	if scope.LibraryID != "" {
		where += `
		AND EXISTS (SELECT 1 FROM editions le JOIN assets la ON la.edition_id = le.id
			WHERE le.work_id = w.id AND la.library_id = ?)`
		args = append(args, scope.LibraryID)
	}
	if scope.WorkID != "" {
		where += ` AND w.id = ?`
		args = append(args, scope.WorkID)
	}
	if scope.Author != "" {
		where += ` AND (json_extract(w.attributes,'$.author') = ? OR json_extract(w.attributes,'$.artist') = ?)`
		args = append(args, scope.Author, scope.Author)
	}
	//nolint:gosec // where is enrichDueWhere plus the literal fragments above; every value is bound
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT w.id FROM works w
		WHERE `+where+`
		ORDER BY w.sort_title, w.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing works needing enrichment: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("catalog: reading works needing enrichment: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// EnrichBacklog counts held music/book Works that are under-enriched (per
// enrichDueWhere), regardless of schedule — what an enrich status endpoint reports
// as "still to do". A companion count of the total held music/book Works lets a
// caller show progress.
func (c *Catalog) EnrichBacklog(ctx context.Context) (bare, total int, err error) {
	row := c.db.Reader().QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM works w WHERE `+enrichDueWhere+`),
			(SELECT count(*) FROM works w WHERE w.content_type IN ('music','book')
				AND EXISTS (SELECT 1 FROM editions e JOIN assets a ON a.edition_id = e.id
					WHERE e.work_id = w.id AND a.source_class = 'managed'
					  AND a.blob_hash IS NOT NULL AND a.missing_since IS NULL))`)
	if err := row.Scan(&bare, &total); err != nil {
		return 0, 0, fmt.Errorf("catalog: counting the enrich backlog: %w", err)
	}
	return bare, total, nil
}
