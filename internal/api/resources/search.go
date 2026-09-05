package resources

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// errNeedQueryOrType is the refusal when a search names nothing to search on.
var errNeedQueryOrType = errors.New("give a query or a content_type to search on")

// Content-intent search (§55, M12, #396) — find a work by what it IS, with no
// source in the question.
//
// The follow surface is source-agnostic, and so is this: a caller searches by
// title and intent, never by which indexer or feed to ask. It resolves against
// the LIBRARY's works — the normalised sort_title a scan records — so "the
// conversation" finds "The Conversation" without the caller knowing how
// identification normalises anything. It is a read; it is a POST only because
// the intent travels in a body, the same reason /quality-profiles/{id}/evaluate
// is a POST under the read floor.

// SearchContentRequest is the intent behind POST /search: find works by title
// and/or content type.
type SearchContentRequest struct {
	Query       string `json:"query"`
	ContentType string `json:"content_type"`
	Limit       int    `json:"limit"`
}

// WorkSummary is one work a content-intent search returned — enough to want it
// or follow it by id rather than by a description that might create a second work.
type WorkSummary struct {
	WorkID      string `json:"work_id"`
	ContentType string `json:"content_type"`
	Title       string `json:"title"`
	Year        int    `json:"year,omitempty"`
	// TVDBID is the work's stored TVDB external id (ADR-0050), when it has one —
	// the feed identity follow_source takes as tvdb_id, so a client can follow a
	// search result in ONE step rather than by a title that might create a second
	// work. It is the value the metadata provider resolved when the work entered
	// the library; a work with no stored tvdb id omits it, and the client then
	// asks the operator for one (decision 2, M12). This surfaces an id already in
	// the library — it is NOT a live TVDB discovery search for series the library
	// does not yet hold, which is a separate future metadata-search mode.
	TVDBID string `json:"tvdb_id,omitempty"`
	// Attributes are the work's per-type facts (an artist, an author, a series)
	// and Artwork its poster (ADR-0075) — what a search result row draws. Artwork
	// is null when the work has none the caller may see.
	Attributes json.RawMessage `json:"attributes"`
	Artwork    *ArtworkRef     `json:"artwork"`
}

// EpisodeHit is a part of a work that matched — an episode (a series EDITION
// a scan produced, with its file) or an ITEM a followed source projected
// (ADR-0056; bytes may or may not have landed). Both are returned because a
// person searching "pilot" means the episode, not the series.
type EpisodeHit struct {
	Kind        string `json:"kind"` // "edition" | "item"
	ID          string `json:"id"`
	WorkID      string `json:"work_id"`
	WorkTitle   string `json:"work_title"`
	ContentType string `json:"content_type"`
	Title       string `json:"title"`
	Season      *int64 `json:"season"`
	Episode     *int64 `json:"episode"`
	// PrimaryAsset is the episode's playable file — present on an edition hit
	// that holds bytes, absent on an item (which has none of its own).
	PrimaryAsset *PrimaryAssetRef `json:"primary_asset,omitempty"`
}

// SearchResult is what both search doors return (ADR-0075): the works that
// matched, and the episodes that matched. Neither list is ever null.
type SearchResult struct {
	Works    []WorkSummary `json:"works"`
	Episodes []EpisodeHit  `json:"episodes"`
}

const (
	searchDefaultLimit = 25
	searchMaxLimit     = 100
)

// SearchContent runs a content-intent library search — the ONE implementation
// behind POST /search and the MCP search_content tool (ADR-0075), so the two
// doors cannot drift. The guest boundary rides the context: a Guest's hits
// carry no vault poster and no vault file.
func (a *API) SearchContent(ctx context.Context, req SearchContentRequest) (SearchResult, error) {
	query := strings.TrimSpace(req.Query)
	contentType := strings.ToLower(strings.TrimSpace(req.ContentType))
	if query == "" && contentType == "" {
		return SearchResult{}, &badRequest{errNeedQueryOrType}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}

	works, err := a.searchWorks(ctx, query, contentType, limit)
	if err != nil {
		return SearchResult{}, err
	}
	out := SearchResult{Works: works, Episodes: []EpisodeHit{}}
	// A content_type-only search is a listing of works; episodes need words.
	if query == "" {
		return out, nil
	}
	editions, err := a.searchEpisodeEditions(ctx, query, contentType, limit)
	if err != nil {
		return SearchResult{}, err
	}
	items, err := a.searchItems(ctx, query, contentType, limit)
	if err != nil {
		return SearchResult{}, err
	}
	out.Episodes = append(append(out.Episodes, editions...), items...)
	return out, nil
}

func (a *API) searchWorks(ctx context.Context, query, contentType string, limit int) ([]WorkSummary, error) {
	art := artworkPick(ctx, "works.id")
	args := append([]any{}, art.args...)
	where := []string{"1 = 1"}
	if query != "" {
		where = append(where, "works.sort_title LIKE ?")
		args = append(args, "%"+strings.ToLower(query)+"%")
	}
	if contentType != "" {
		where = append(where, "works.content_type = ?")
		args = append(args, contentType)
	}
	args = append(args, limit)

	// The tvdb id is a correlated subquery rather than a JOIN so the result stays
	// one row per work: a work could in principle carry more than one tvdb row,
	// and a JOIN would multiply it. LIMIT 1 takes the lexically-first, which is
	// deterministic; the common case is zero or one row (ADR-0050, decision 2).
	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	stmt := `SELECT works.id, works.content_type, works.title, works.year, works.attributes,
		(SELECT value FROM external_ids
		 WHERE entity_type = 'work' AND entity_id = works.id AND source = 'tvdb'
		 ORDER BY value LIMIT 1) AS tvdb_id, ` + artworkColumns + `
		FROM works LEFT JOIN assets art ON art.id = ` + art.sql + `
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY works.sort_title, works.id LIMIT ?`

	rows, err := a.reader.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []WorkSummary{}
	for rows.Next() {
		var (
			w      WorkSummary
			year   sql.NullInt64
			attrs  string
			tvdbID sql.NullString
			as     artworkScan
		)
		dest := append([]any{&w.WorkID, &w.ContentType, &w.Title, &year, &attrs, &tvdbID}, as.dests()...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if year.Valid {
			w.Year = int(year.Int64)
		}
		if tvdbID.Valid {
			w.TVDBID = tvdbID.String
		}
		w.Attributes = json.RawMessage(attrs)
		w.Artwork = as.ref()
		out = append(out, w)
	}
	return out, rows.Err()
}

// searchEpisodeEditions matches the episode_title a series scan wrote on an
// edition. The file is the edition's own primary asset.
func (a *API) searchEpisodeEditions(ctx context.Context, query, contentType string, limit int) ([]EpisodeHit, error) {
	pa := editionPrimaryPick(ctx, "e.id")
	args := append([]any{}, pa.args...)
	where := []string{"lower(json_extract(e.attributes, '$.episode_title')) LIKE ?"}
	args = append(args, "%"+strings.ToLower(query)+"%")
	if contentType != "" {
		where = append(where, "w.content_type = ?")
		args = append(args, contentType)
	}
	args = append(args, limit)
	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	stmt := `SELECT e.id, w.id, w.title, w.content_type,
		COALESCE(json_extract(e.attributes, '$.episode_title'), ''),
		CAST(json_extract(e.attributes, '$.season') AS INTEGER),
		CAST(json_extract(e.attributes, '$.episode') AS INTEGER), ` + primaryColumns + `
		FROM editions e JOIN works w ON w.id = e.work_id` + primaryJoins(pa) + `
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY w.sort_title, e.id LIMIT ?`
	rows, err := a.reader.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []EpisodeHit{}
	for rows.Next() {
		var (
			h               EpisodeHit
			season, episode sql.NullInt64
			ps              primaryScan
		)
		dest := append([]any{&h.ID, &h.WorkID, &h.WorkTitle, &h.ContentType, &h.Title, &season, &episode}, ps.dests()...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		h.Kind = "edition"
		h.Season, h.Episode = nullInt(season), nullInt(episode)
		h.PrimaryAsset = ps.ref()
		out = append(out, h)
	}
	return out, rows.Err()
}

// searchItems matches the title of an item a followed source projected
// (ADR-0056). An item has no file of its own; a client that wants to play it
// goes through the work's editions.
func (a *API) searchItems(ctx context.Context, query, contentType string, limit int) ([]EpisodeHit, error) {
	where := []string{"lower(i.title) LIKE ?"}
	args := []any{"%" + strings.ToLower(query) + "%"}
	if contentType != "" {
		where = append(where, "w.content_type = ?")
		args = append(args, contentType)
	}
	args = append(args, limit)
	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	stmt := `SELECT i.id, w.id, w.title, w.content_type, i.title,
		CAST(json_extract(i.attributes, '$.season') AS INTEGER),
		CAST(json_extract(i.attributes, '$.episode') AS INTEGER)
		FROM items i JOIN works w ON w.id = i.work_id
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY w.sort_title, i.published_at DESC, i.id LIMIT ?`
	rows, err := a.reader.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []EpisodeHit{}
	for rows.Next() {
		var (
			h               EpisodeHit
			season, episode sql.NullInt64
		)
		if err := rows.Scan(&h.ID, &h.WorkID, &h.WorkTitle, &h.ContentType, &h.Title, &season, &episode); err != nil {
			return nil, err
		}
		h.Kind = "item"
		h.Season, h.Episode = nullInt(season), nullInt(episode)
		out = append(out, h)
	}
	return out, rows.Err()
}

// searchContentRoute is POST /api/v1/search — a shell over SearchContent.
func (a *API) searchContentRoute(w http.ResponseWriter, r *http.Request) {
	var body SearchContentRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	out, err := a.SearchContent(r.Context(), body)
	if err != nil {
		var bad *badRequest
		if errors.As(err, &bad) {
			httpapi.Fail(w, r, problem.BadRequest(bad.err.Error()))
			return
		}
		a.fail(w, r, "work", err)
		return
	}
	a.write(w, r, http.StatusOK, out)
}
