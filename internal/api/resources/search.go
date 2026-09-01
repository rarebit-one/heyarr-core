package resources

import (
	"context"
	"database/sql"
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
}

const (
	searchDefaultLimit = 25
	searchMaxLimit     = 100
)

// SearchContent runs a content-intent library search. Exported so a second door
// could share it; the REST route is its only caller today, and the MCP
// search_content tool keeps its own truncatable envelope (it predates this).
func (a *API) SearchContent(ctx context.Context, req SearchContentRequest) ([]WorkSummary, error) {
	query := strings.TrimSpace(req.Query)
	contentType := strings.ToLower(strings.TrimSpace(req.ContentType))
	if query == "" && contentType == "" {
		return nil, &badRequest{errNeedQueryOrType}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}

	where := []string{"1 = 1"}
	args := []any{}
	if query != "" {
		where = append(where, "sort_title LIKE ?")
		args = append(args, "%"+strings.ToLower(query)+"%")
	}
	if contentType != "" {
		where = append(where, "content_type = ?")
		args = append(args, contentType)
	}
	args = append(args, limit)

	// The tvdb id is a correlated subquery rather than a JOIN so the result stays
	// one row per work: a work could in principle carry more than one tvdb row,
	// and a JOIN would multiply it. LIMIT 1 takes the lexically-first, which is
	// deterministic; the common case is zero or one row (ADR-0050, decision 2).
	//nolint:gosec // assembled only from the literal fragments above; every value is bound
	stmt := `SELECT id, content_type, title, year,
		(SELECT value FROM external_ids
		 WHERE entity_type = 'work' AND entity_id = works.id AND source = 'tvdb'
		 ORDER BY value LIMIT 1) AS tvdb_id
		FROM works WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY sort_title, id LIMIT ?`

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
			tvdbID sql.NullString
		)
		if err := rows.Scan(&w.WorkID, &w.ContentType, &w.Title, &year, &tvdbID); err != nil {
			return nil, err
		}
		if year.Valid {
			w.Year = int(year.Int64)
		}
		if tvdbID.Valid {
			w.TVDBID = tvdbID.String
		}
		out = append(out, w)
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
	a.write(w, r, http.StatusOK, map[string]any{"works": out})
}
