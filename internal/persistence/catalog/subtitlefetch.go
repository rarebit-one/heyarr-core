package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The subtitle fetch schedule (ADR-0085): which subtitle wants are due a fetch,
// and the bookkeeping that paces the provider. The sibling of the search
// schedule (searchschedule.go) for direct-route subtitle wants, which the search
// beat never touches.

// DueSubtitleFetch is one subtitle want the fetch beat should ask a provider
// about now. It carries what the fetch job needs to build a SubtitleQuery
// without re-reading the want.
type DueSubtitleFetch struct {
	DesiredItemID string
	// Language the want asks for, an ISO-639-1 code.
	Language string
	// IMDBID/TMDBID identify the work (the series or film) at an external
	// service — the digits, with any "tt" prefix stripped. Either or both may be
	// empty; a want with neither cannot be looked up and is not returned.
	IMDBID string
	TMDBID string
	// Season/Episode narrow a series query to one episode, 0 when the want is not
	// an episode (a film's edition-scoped want).
	Season  int
	Episode int
	// Fruitless is how many consecutive prior fetches found nothing — the backoff
	// exponent.
	Fruitless int
}

// DueSubtitleFetches lists the subtitle wants due a provider fetch as of now.
//
// A want qualifies when it is a subtitle-aspect want resting in MISSING (idle,
// content not yet satisfied), its PRIMARY content is held (a managed non-subtitle
// asset exists on its target — you cannot caption a video you do not have, and
// matching a subtitle needs the release), it has at least one external id to look
// up by, and its schedule says it is due (or it has never been fetched). Ordered
// by how overdue each is, so a limit truncates the least urgent.
func (c *Catalog) DueSubtitleFetches(ctx context.Context, now time.Time, limit int) ([]DueSubtitleFetch, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT d.id, d.language,
		       coalesce(imdb.value, ''), coalesce(tmdb.value, ''),
		       coalesce(json_extract(it.attributes, '$.season'), ''),
		       coalesce(json_extract(it.attributes, '$.episode'), ''),
		       coalesce(s.fruitless, 0)
		FROM desired_items d
		JOIN acquisition_state a ON a.desired_item_id = d.id
		LEFT JOIN items it ON it.id = d.item_id
		LEFT JOIN external_ids imdb
		       ON imdb.entity_type = 'work' AND imdb.entity_id = d.work_id AND imdb.source = 'imdb'
		LEFT JOIN external_ids tmdb
		       ON tmdb.entity_type = 'work' AND tmdb.entity_id = d.work_id AND tmdb.source = 'tmdb'
		LEFT JOIN subtitle_fetch_schedule s ON s.desired_item_id = d.id
		WHERE d.aspect = 'subtitle'
		  AND a.phase = 'idle'
		  AND a.content != 'satisfied'
		  AND (imdb.value IS NOT NULL OR tmdb.value IS NOT NULL)
		  AND (s.next_fetch_at IS NULL OR s.next_fetch_at <= ?)
		  AND EXISTS (
		        SELECT 1 FROM assets v
		        WHERE v.edition_id = coalesce(it.edition_id, d.edition_id)
		          AND v.role != 'subtitle'
		          AND v.source_class = 'managed'
		          AND v.blob_hash IS NOT NULL
		          AND v.missing_since IS NULL
		          AND (d.item_id IS NULL OR v.item_id = d.item_id OR v.item_id IS NULL)
		  )
		ORDER BY coalesce(s.next_fetch_at, ''), d.id
		LIMIT ?`, sortable(now), limit)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing subtitle wants due a fetch: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DueSubtitleFetch
	for rows.Next() {
		var (
			d                 DueSubtitleFetch
			seasonStr, epsStr string
		)
		if err := rows.Scan(&d.DesiredItemID, &d.Language, &d.IMDBID, &d.TMDBID,
			&seasonStr, &epsStr, &d.Fruitless); err != nil {
			return nil, fmt.Errorf("catalog: reading subtitle wants due a fetch: %w", err)
		}
		d.IMDBID = strings.TrimPrefix(strings.TrimSpace(d.IMDBID), "tt")
		d.TMDBID = strings.TrimSpace(d.TMDBID)
		d.Season = atoiOrZero(seasonStr)
		d.Episode = atoiOrZero(epsStr)
		out = append(out, d)
	}
	return out, rows.Err()
}

// SubtitleFetchContext is everything the fetch job needs to fetch one want's
// subtitle, re-read at handling time so nothing carried in the payload goes
// stale. ok is false when the want is no longer eligible — not a subtitle want,
// or its source video is gone — in which case the job is a no-op.
type SubtitleFetchContext struct {
	DesiredItemID      string
	Language           string
	IMDBID             string
	TMDBID             string
	Season             int
	Episode            int
	SourceVideoAssetID string
	Fruitless          int
}

// SubtitleFetchContext reads one subtitle want's fetch context: its language,
// the work's external ids (imdb without the "tt" prefix, tmdb), the episode's
// season/episode, its fruitless streak, and the source video to attach to. It is
// the single-want read the fetch job makes; DueSubtitleFetches is the batch read
// the beat makes.
func (c *Catalog) SubtitleFetchContext(ctx context.Context, desiredItemID string) (SubtitleFetchContext, bool, error) {
	out := SubtitleFetchContext{DesiredItemID: desiredItemID}
	var seasonStr, epsStr string
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT d.language,
		       coalesce(imdb.value, ''), coalesce(tmdb.value, ''),
		       coalesce(json_extract(it.attributes, '$.season'), ''),
		       coalesce(json_extract(it.attributes, '$.episode'), ''),
		       coalesce(s.fruitless, 0)
		FROM desired_items d
		LEFT JOIN items it ON it.id = d.item_id
		LEFT JOIN external_ids imdb
		       ON imdb.entity_type = 'work' AND imdb.entity_id = d.work_id AND imdb.source = 'imdb'
		LEFT JOIN external_ids tmdb
		       ON tmdb.entity_type = 'work' AND tmdb.entity_id = d.work_id AND tmdb.source = 'tmdb'
		LEFT JOIN subtitle_fetch_schedule s ON s.desired_item_id = d.id
		WHERE d.id = ? AND d.aspect = 'subtitle'`, desiredItemID).
		Scan(&out.Language, &out.IMDBID, &out.TMDBID, &seasonStr, &epsStr, &out.Fruitless)
	if errors.Is(err, sql.ErrNoRows) {
		return SubtitleFetchContext{}, false, nil
	}
	if err != nil {
		return SubtitleFetchContext{}, false, fmt.Errorf("catalog: reading subtitle fetch context for %s: %w", desiredItemID, err)
	}
	out.IMDBID = strings.TrimPrefix(strings.TrimSpace(out.IMDBID), "tt")
	out.TMDBID = strings.TrimSpace(out.TMDBID)
	out.Season = atoiOrZero(seasonStr)
	out.Episode = atoiOrZero(epsStr)

	video, ok, err := c.SourceVideoForSubtitle(ctx, desiredItemID)
	if err != nil {
		return SubtitleFetchContext{}, false, err
	}
	if !ok {
		return SubtitleFetchContext{}, false, nil
	}
	out.SourceVideoAssetID = video
	return out, true, nil
}

// SourceVideoForSubtitle resolves the video asset a subtitle want captions — the
// managed, non-subtitle asset on the want's target. It prefers the asset LINKED
// to the want's item (ADR-0086, an episode's own video) and falls back to a video
// on the target's edition whose item link is unset (pre-ADR-0086 content, or a
// film's edition-scoped want). Returns "" and ok=false when the target holds no
// video — the primary-satisfied gate, read here so the fetch job attaches to the
// right stem or does nothing.
func (c *Catalog) SourceVideoForSubtitle(ctx context.Context, desiredItemID string) (assetID string, ok bool, err error) {
	err = c.db.Reader().QueryRowContext(ctx, `
		SELECT v.id
		FROM desired_items d
		LEFT JOIN items it ON it.id = d.item_id
		JOIN assets v
		  ON v.edition_id = coalesce(it.edition_id, d.edition_id)
		 AND v.role != 'subtitle'
		 AND v.source_class = 'managed'
		 AND v.blob_hash IS NOT NULL
		 AND v.missing_since IS NULL
		 AND (d.item_id IS NULL OR v.item_id = d.item_id OR v.item_id IS NULL)
		WHERE d.id = ?
		ORDER BY (d.item_id IS NOT NULL AND v.item_id = d.item_id) DESC, v.id
		LIMIT 1`, desiredItemID).Scan(&assetID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("catalog: resolving the source video for %s: %w", desiredItemID, err)
	}
	return assetID, true, nil
}

// RecordSubtitleFetchScheduled records that a want was fetched and when it is due
// again — the fetch beat's bookkeeping, the sibling of RecordSearchScheduled. No
// event: enqueuing the job is the transition the queue already emits, and a
// second event per want per pass would turn the log into a heartbeat.
func (c *Catalog) RecordSubtitleFetchScheduled(
	ctx context.Context, desiredItemID string, fruitless int, now, next time.Time,
) error {
	if desiredItemID == "" {
		return fmt.Errorf("catalog: recording a scheduled subtitle fetch needs a want")
	}
	nowStr, nextStr := sortable(now), sortable(next)
	_, err := c.db.Writer().ExecContext(ctx, `
		INSERT INTO subtitle_fetch_schedule
			(desired_item_id, fruitless, last_fetched_at, next_fetch_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (desired_item_id) DO UPDATE SET
			fruitless       = excluded.fruitless,
			last_fetched_at = excluded.last_fetched_at,
			next_fetch_at   = excluded.next_fetch_at,
			updated_at      = excluded.updated_at`,
		desiredItemID, fruitless, nowStr, nextStr, nowStr, nowStr)
	if err != nil {
		return fmt.Errorf("catalog: recording a scheduled subtitle fetch: %w", err)
	}
	return nil
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
