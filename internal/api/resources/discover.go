package resources

import (
	"context"
	"errors"
	"net/http"
	"strings"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Discovery search (§55, M12, #451) — the "not-yet-in-library" door.
//
// # Why this is a SECOND door beside /search, not a mode of it
//
// POST /search is a library search: it answers "what do I already hold that
// matches this", by a LIKE over works.sort_title, offline and fast. It cannot
// answer "what could I acquire that I do NOT hold", because the library is the
// only thing it reads. Discovery asks the metadata provider instead — a live
// lookup against TVDB (ADR-0050/0058) that returns candidate works with their
// external ids, whether or not the library has ever seen them — so a caller can
// go from a free-text title to a follow_source in one step.
//
// Keeping them separate keeps each honest about its cost and its answer: /search
// stays the fast offline path a client hits on every keystroke, and /discover is
// the deliberate "reach out and look" a client hits when the library came back
// empty. A `mode=discover` flag on /search would have made one route sometimes
// touch the network and sometimes not, which is the quiet-latency knob this
// codebase avoids.
//
// It is a POST under the read floor for the same reason /search and
// /quality-profiles/{id}/evaluate are: the intent travels in a body, and it
// writes nothing — a discovery search enters nothing into the library, it only
// tells the caller what a follow WOULD name.

// errNoDiscovery is the refusal when this node has no provider that can look
// content up. It is neither the caller's fault nor a failure — there is simply
// nothing configured to answer — so the route renders it 503, not 400 or 500.
var errNoDiscovery = errors.New(
	"no metadata provider is configured that can search for new content — " +
		"configure a TVDB provider (ADR-0058) to enable discovery")

// DiscoverRequest is the intent behind POST /discover: find candidate works by
// free text, including ones the library does not hold.
type DiscoverRequest struct {
	Query string `json:"query"`
}

// DiscoveryResult is one candidate a discovery search returned — enough to show
// a person which work it is and to follow it by id in one step.
type DiscoveryResult struct {
	// Title and Year name the work as the metadata service knows it.
	Title string `json:"title"`
	Year  int    `json:"year,omitempty"`
	// Type is the followed source type this candidate would be followed as
	// (tv_series today), so a client picks the right follow flow without
	// inferring it from the id's shape.
	Type string `json:"type"`
	// TVDBID is the candidate's TVDB series id — the value follow_source takes as
	// tvdb_id, so a client follows a discovery result in ONE step rather than by
	// a title that might create a second work. Present for a tv_series candidate;
	// a future provider whose external id is not a TVDB id would carry its own
	// field rather than overloading this one.
	TVDBID string `json:"tvdb_id,omitempty"`
	// Overview is a short human description when the service supplied one, so a
	// person choosing between two same-named series has something to choose on.
	Overview string `json:"overview,omitempty"`
}

// Discover runs a live metadata-provider search for candidate works (#451).
// Exported and shared by POST /api/v1/discover and MCP's discover_content, the
// same "one intent, two doors" discipline SearchContent's sibling FollowSource
// is built on.
//
// It fans out across every discovery-capable metadata provider and merges the
// candidates, deduplicated by (type, external id) so two providers offering the
// same series yield one result. A provider that errors is logged and skipped —
// one unreachable service must not discard what another returned — but if EVERY
// provider errored (and none answered), the error is surfaced rather than
// reported as an empty catalogue. With no discovery-capable provider at all it
// returns errNoDiscovery, which the doors render as "not available", not "found
// nothing".
func (a *API) Discover(ctx context.Context, req DiscoverRequest) ([]DiscoveryResult, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, &badRequest{errors.New("give a query to discover on")}
	}

	searchers := a.providers.DiscoverySearchers()
	if len(searchers) == 0 {
		return nil, errNoDiscovery
	}

	out := []DiscoveryResult{}
	seen := map[string]bool{}
	var answered bool
	var lastErr error
	for _, s := range searchers {
		candidates, err := s.Discover(ctx, query)
		if err != nil {
			// A failed lookup is a call failure, not an empty catalogue (see
			// providers.DiscoverySearcher). Keep the others' answers; remember
			// the error so a total failure is surfaced rather than swallowed.
			a.log.Warn("a discovery provider failed",
				"provider", s.Name(), "error", err)
			lastErr = err
			continue
		}
		answered = true
		for _, c := range candidates {
			key := string(c.Type) + "\x00" + c.ExternalID
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, discoveryResultFor(c))
		}
	}
	if !answered {
		// Every provider errored. Surface it — the caller asked a question that
		// could not be answered, which is different from one whose answer is
		// "nothing".
		return nil, lastErr
	}
	return out, nil
}

// discoveryResultFor projects a neutral candidate onto the wire shape. The TVDB
// id is surfaced as tvdb_id only for a tv_series candidate, matching how
// WorkSummary and follow_source spell that identity.
func discoveryResultFor(c providers.DiscoveryCandidate) DiscoveryResult {
	r := DiscoveryResult{
		Title:    c.Title,
		Year:     c.Year,
		Type:     string(c.Type),
		Overview: c.Overview,
	}
	if c.Type == "tv_series" {
		r.TVDBID = c.ExternalID
	}
	return r
}

// DiscoverClientFault classifies a Discover error for the MCP door, the sibling
// of ClientFault and FollowClientFault: it maps a caller's fault (an empty
// query) to a message and true, and reports "no provider configured" as a client
// fault too — it is actionable by the operator and quotable to the agent, unlike
// a 500. A provider's own call failure stays ours.
func DiscoverClientFault(err error) (string, bool) {
	var bad *badRequest
	switch {
	case errors.As(err, &bad):
		return bad.err.Error(), true
	case errors.Is(err, errNoDiscovery):
		return errNoDiscovery.Error(), true
	}
	return "", false
}

// discoverRoute is POST /api/v1/discover — a shell over Discover.
func (a *API) discoverRoute(w http.ResponseWriter, r *http.Request) {
	var body DiscoverRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	out, err := a.Discover(r.Context(), body)
	if err != nil {
		var bad *badRequest
		switch {
		case errors.As(err, &bad):
			httpapi.Fail(w, r, problem.BadRequest(bad.err.Error()))
		case errors.Is(err, errNoDiscovery):
			httpapi.Fail(w, r, problem.ServiceUnavailable(errNoDiscovery.Error()))
		default:
			a.fail(w, r, "discovery", err)
		}
		return
	}
	a.write(w, r, http.StatusOK, map[string]any{"results": out})
}
