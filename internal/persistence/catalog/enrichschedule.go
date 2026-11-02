package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// The enrich schedule (ADR-0087): which held Works are due an enrich pass, and
// the bookkeeping that paces the providers. The sibling of the subtitle fetch
// schedule (subtitlefetch.go) for a job over a Work rather than a want.
//
// Enrichment covers music and books (MusicBrainz, Open Library — ADR-0087) and
// films and series (TMDB — ADR-0122). The predicate below is per content type
// where the content types differ: which external-id source counts as
// "identified", and whether a Work owes a backdrop as well as a poster.

// DueEnrichWork is one held Work the enrich beat should ask a provider about now.
// It carries what the enrich job needs to build an EnrichQuery without re-reading
// the Work.
type DueEnrichWork struct {
	WorkID      string
	ContentType string // "music" | "book" | "movie" | "series"
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
	// HasPoster and HasBackdrop report which artwork faces the Work already
	// resolves — a shipped poster.jpg, a cover lifted from the book file, or an
	// image an earlier pass fetched. The worker fetches only what is missing, so
	// a re-run never downloads bytes the node already holds and a fetched image
	// never displaces one the release shipped (ADR-0105). Read by EnrichContext
	// at handling time; the beat's due listing leaves them zero.
	HasPoster   bool
	HasBackdrop bool
	// ExternalIDs is every id already recorded for the Work (source → value), the
	// strong keys a provider looks up by before it falls back to a title search
	// — a followed series' tvdb id, a wanted film's tmdb id. Read by
	// EnrichContext; the beat's due listing leaves it nil.
	ExternalIDs map[string]string
}

// enrichContentTypes is the SQL set of content types enrichment covers.
const enrichContentTypes = `('music','book','movie','series')`

// enrichSourceCase names, per content type, the external-id source whose
// presence means the Work has been identified by its enrich authority. Keeping
// it in one place is what lets the due predicate, the backlog count and the
// worker agree on what "has an id" means for each kind.
const enrichSourceCase = `CASE w.content_type
	WHEN 'music' THEN 'musicbrainz'
	WHEN 'book' THEN 'openlibrary'
	ELSE 'tmdb' END`

// Artwork faces, by filename: the shared classes in artworkclass.go, bound to
// the `aw` alias the predicates below use. The enrich side names a fetched poster
// cover.jpg and a fetched backdrop backdrop.jpg (enrich.go) so the two land in the
// right class.
var (
	artworkIsDecoration = ArtworkIsNotAPosterSQL("aw.filename")
	artworkIsBackdrop   = ArtworkIsBackdropSQL("aw.filename")
)

// enrichHeldWhere is "a Work enrichment covers that is HELD": a managed asset
// with bytes exists on one of its editions. A wanted-but-unacquired Work is not
// enriched — there is nothing yet to decorate.
const enrichHeldWhere = `
	w.content_type IN ` + enrichContentTypes + `
	AND EXISTS (
		SELECT 1 FROM editions e JOIN assets a ON a.edition_id = e.id
		WHERE e.work_id = w.id AND a.source_class = 'managed'
		  AND a.blob_hash IS NOT NULL AND a.missing_since IS NULL
	)`

// enrichLackingWhere is "under-enriched": the Work lacks a front-facing image
// (a poster or cover — decoration alone does not count), OR, for a film or
// series, lacks a backdrop, OR lacks the id its enrich authority writes. A Work
// with all of them is fully enriched and drops out; one missing any stays
// eligible, paced by the schedule's backoff.
var enrichLackingWhere = `(
		NOT EXISTS (
			SELECT 1 FROM editions e2 JOIN assets aw ON aw.edition_id = e2.id
			WHERE e2.work_id = w.id AND aw.role = 'artwork'
			  AND aw.blob_hash IS NOT NULL AND aw.missing_since IS NULL
			  AND NOT ` + artworkIsDecoration + `
		)
		OR (w.content_type IN ('movie','series') AND NOT EXISTS (
			SELECT 1 FROM editions e3 JOIN assets aw ON aw.edition_id = e3.id
			WHERE e3.work_id = w.id AND aw.role = 'artwork'
			  AND aw.blob_hash IS NOT NULL AND aw.missing_since IS NULL
			  AND ` + artworkIsBackdrop + `
		))
		OR NOT EXISTS (
			SELECT 1 FROM external_ids x
			WHERE x.entity_type = 'work' AND x.entity_id = w.id
			  AND x.source = (` + enrichSourceCase + `)
		)
	)`

// enrichDueWhere is the shared predicate for "a held Work that is
// under-enriched" (ADR-0087's "no artwork and/or no enrich id", per content type).
var enrichDueWhere = enrichHeldWhere + `
	AND ` + enrichLackingWhere

// DueEnrichWorks lists the held Works due an enrich pass as of now.
// A Work qualifies per enrichDueWhere and when its schedule says it is due (or it
// has never been enriched). Ordered by how overdue each is, so a limit truncates
// the least urgent.
func (c *Catalog) DueEnrichWorks(ctx context.Context, now time.Time, limit int) ([]DueEnrichWork, error) {
	if limit <= 0 {
		return nil, nil
	}
	//nolint:gosec // the concatenated fragments are enrichDueWhere and enrichBackoff's literal table and columns; every value is bound
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT w.id, w.content_type, w.title, coalesce(w.year, 0),
		       coalesce(json_extract(w.attributes, '$.artist'), ''),
		       coalesce(json_extract(w.attributes, '$.author'), ''),
		       coalesce(s.fruitless, 0)
		FROM works w
		`+enrichBackoff.leftJoin("w.id")+`
		WHERE `+enrichDueWhere+`
		  AND `+enrichBackoff.dueWhere()+`
		ORDER BY `+enrichBackoff.dueOrder()+`, w.id
		LIMIT ?`, sqlite.FormatTimestamp(now), limit)
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
// nothing carried in the payload goes stale: its identity, what artwork it
// already resolves and the ids already recorded for it. ok is false when the
// Work is gone or is not a Work enrichment covers — in which case the job is a
// no-op. It does NOT re-check the under-enriched predicate: a Work enriched
// between enqueue and handling simply finds everything present, fetches
// nothing, and reschedules far out.
func (c *Catalog) EnrichContext(ctx context.Context, workID string) (DueEnrichWork, bool, error) {
	var d DueEnrichWork
	d.WorkID = workID
	var hasPoster, hasBackdrop int
	//nolint:gosec // the concatenated fragments are enrichBackoff's literal join and the artwork predicates above; the work id is bound
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT w.content_type, w.title, coalesce(w.year, 0),
		       coalesce(json_extract(w.attributes, '$.artist'), ''),
		       coalesce(json_extract(w.attributes, '$.author'), ''),
		       coalesce(s.fruitless, 0),
		       EXISTS (
		         SELECT 1 FROM editions e2 JOIN assets aw ON aw.edition_id = e2.id
		         WHERE e2.work_id = w.id AND aw.role = 'artwork'
		           AND aw.blob_hash IS NOT NULL AND aw.missing_since IS NULL
		           AND NOT `+artworkIsDecoration+`),
		       EXISTS (
		         SELECT 1 FROM editions e3 JOIN assets aw ON aw.edition_id = e3.id
		         WHERE e3.work_id = w.id AND aw.role = 'artwork'
		           AND aw.blob_hash IS NOT NULL AND aw.missing_since IS NULL
		           AND `+artworkIsBackdrop+`)
		FROM works w
		`+enrichBackoff.leftJoin("w.id")+`
		WHERE w.id = ? AND w.content_type IN `+enrichContentTypes, workID).
		Scan(&d.ContentType, &d.Title, &d.Year, &d.Artist, &d.Author, &d.Fruitless, &hasPoster, &hasBackdrop)
	if errors.Is(err, sql.ErrNoRows) {
		return DueEnrichWork{}, false, nil
	}
	if err != nil {
		return DueEnrichWork{}, false, fmt.Errorf("catalog: reading enrich context for %s: %w", workID, err)
	}
	d.HasPoster = hasPoster == 1
	d.HasBackdrop = hasBackdrop == 1

	// The ids already known, first value per source — the same determinism rule
	// the API's external_ids projection takes.
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT source, value FROM external_ids
		WHERE entity_type = 'work' AND entity_id = ? ORDER BY source, value`, workID)
	if err != nil {
		return DueEnrichWork{}, false, fmt.Errorf("catalog: reading external ids for %s: %w", workID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var source, value string
		if err := rows.Scan(&source, &value); err != nil {
			return DueEnrichWork{}, false, fmt.Errorf("catalog: reading external ids for %s: %w", workID, err)
		}
		if d.ExternalIDs == nil {
			d.ExternalIDs = map[string]string{}
		}
		if _, seen := d.ExternalIDs[source]; !seen {
			d.ExternalIDs[source] = value
		}
	}
	if err := rows.Err(); err != nil {
		return DueEnrichWork{}, false, fmt.Errorf("catalog: reading external ids for %s: %w", workID, err)
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
	_, err := enrichBackoff.record(ctx, c.db, backoffAttempt{
		subject: workID, fruitless: fruitless, now: now, next: next,
	})
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
	// ContentType narrows to one of the content types enrichment covers
	// ("movie", "series", "music", "book"), so an operator who has just wired
	// TMDB can backfill the films without re-asking Open Library about every
	// book. The caller validates the value; an unknown one here simply matches
	// nothing.
	ContentType string
	// Author matches the Work's attributes.author (or attributes.artist) exactly —
	// the shelf-name grouping list_authors/list_artists show, so an operator can
	// enrich "everything filed under Reads" in one call.
	Author string
	All    bool
}

// WorksNeedingEnrich returns the ids of held Works that are under-enriched (per
// enrichDueWhere) within a scope, IGNORING the backoff schedule — the "do it
// now" set an operator backfill enqueues, distinct from DueEnrichWorks which the
// beat paces. Ordered by title for a stable, readable batch.
func (c *Catalog) WorksNeedingEnrich(ctx context.Context, scope EnrichScope) ([]string, error) {
	where := enrichDueWhere
	var args []any
	if scope.ContentType != "" {
		where += ` AND w.content_type = ?`
		args = append(args, scope.ContentType)
	}
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

// EnrichBacklog is what an enrich status read reports: how many held Works
// enrichment covers are still under-enriched (per enrichDueWhere), regardless of
// schedule, out of how many are held — overall and per content type, so a person
// who has just wired TMDB can watch the films drain without the books in the way.
type EnrichBacklog struct {
	UnderEnriched int
	Total         int
	// ByContentType has an entry for every content type enrichment covers that
	// the node holds at least one Work of.
	ByContentType map[string]EnrichBacklogCount
}

// EnrichBacklogCount is one content type's share of the backlog.
type EnrichBacklogCount struct {
	UnderEnriched int `json:"under_enriched"`
	Total         int `json:"total"`
}

// EnrichBacklogCounts counts the enrich backlog, overall and per content type.
func (c *Catalog) EnrichBacklogCounts(ctx context.Context) (EnrichBacklog, error) {
	//nolint:gosec // the concatenated fragments are the literal predicates above; nothing is bound because nothing is a value
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT w.content_type,
		       sum(CASE WHEN `+enrichLackingWhere+` THEN 1 ELSE 0 END),
		       count(*)
		FROM works w
		WHERE `+enrichHeldWhere+`
		GROUP BY w.content_type
		ORDER BY w.content_type`)
	if err != nil {
		return EnrichBacklog{}, fmt.Errorf("catalog: counting the enrich backlog: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := EnrichBacklog{ByContentType: map[string]EnrichBacklogCount{}}
	for rows.Next() {
		var ct string
		var bare, total int
		if err := rows.Scan(&ct, &bare, &total); err != nil {
			return EnrichBacklog{}, fmt.Errorf("catalog: reading the enrich backlog: %w", err)
		}
		out.ByContentType[ct] = EnrichBacklogCount{UnderEnriched: bare, Total: total}
		out.UnderEnriched += bare
		out.Total += total
	}
	if err := rows.Err(); err != nil {
		return EnrichBacklog{}, fmt.Errorf("catalog: reading the enrich backlog: %w", err)
	}
	return out, nil
}
