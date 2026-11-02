package resources

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// EnrichBackfillRequest is the POST /enrich/backfill body.
//
// Exactly one scope is required: a library, a work, an author, or an explicit
// all. Mandatory rather than defaulting to everything because enriching a whole
// node is a large batch of jobs against public services, and "I meant this one
// author" should not be one forgotten flag from "every book you have".
// ContentType narrows any scope further to one kind — "all the films" after
// wiring TMDB, without re-asking Open Library about every book.
type EnrichBackfillRequest struct {
	LibraryID   string `json:"library_id"`
	WorkID      string `json:"work_id"`
	Author      string `json:"author"`
	ContentType string `json:"content_type"`
	All         bool   `json:"all"`
}

// enrichContentTypes is the closed set a backfill may name: the content types
// enrichment covers. A typo is refused rather than matching nothing, because
// "queued 0 jobs" for `content_type: "movies"` is the silent failure nobody
// looks for.
var enrichContentTypes = []string{"movie", "series", "music", "book"}

// backfillEnrich enqueues an enrich_work job for every held Work in scope that is
// under-enriched (no poster, no backdrop for a film or series, and/or no canonical
// id), ignoring the beat's backoff schedule (ADR-0087, ADR-0122). The beat
// enriches on its own cadence; this is the operator's lever to enrich a scope now
// — after wiring an enrich provider, or after ingesting a shelf of books or a
// batch of films.
//
// "Under-enriched" is the filter on purpose: a Work that already has its artwork
// and id is left alone, so a re-run is cheap and does not re-hit the services for
// nothing. The enrich job is itself idempotent, so a Work enqueued twice (a
// concurrent beat pass) dedupes on its work-scoped key.
func (a *API) backfillEnrich(w http.ResponseWriter, r *http.Request) {
	var body EnrichBackfillRequest
	if !decodeOr400(w, r, &body) {
		return
	}
	if body.LibraryID == "" && body.WorkID == "" && body.Author == "" && !body.All {
		httpapi.Fail(w, r, problem.BadRequest(
			"a scope is required: library_id, work_id, author, or all=true — refusing to guess between one author and the whole node"))
		return
	}
	if body.ContentType != "" && !slices.Contains(enrichContentTypes, body.ContentType) {
		httpapi.Fail(w, r, problem.BadRequest(fmt.Sprintf(
			"content_type must be one of %s, not %q", strings.Join(enrichContentTypes, ", "), body.ContentType)))
		return
	}

	// A node with no enrich provider would enqueue jobs that stay PENDING forever
	// (ADR-0025). That is a legitimate state on a controller-only node, but for an
	// on-demand backfill it is almost always a misconfiguration the operator wants
	// told about rather than silently queued.
	if !a.providers.Has(providers.CapabilityEnrich) {
		httpapi.Fail(w, r, problem.BadRequest(
			"no enrich provider is configured on this node — enrich jobs would never run; configure tmdb (films, series), musicbrainz (music) or openlibrary (books) first"))
		return
	}

	ids, err := a.catalog.WorksNeedingEnrich(r.Context(), catalog.EnrichScope{
		LibraryID: body.LibraryID, WorkID: body.WorkID, Author: body.Author,
		ContentType: body.ContentType, All: body.All,
	})
	if err != nil {
		a.fail(w, r, "work", err)
		return
	}

	enqueued := 0
	for _, id := range ids {
		if _, err := a.jobs.Enqueue(r.Context(), jobs.EnqueueOptions{
			Type:               acquisition.EnrichWorkJobType,
			Payload:            acquisition.EnrichWorkPayload{WorkID: id},
			DedupeKey:          acquisition.EnrichWorkDedupeKey(id),
			RequiredCapability: providers.CapabilityEnrich.JobCapability(),
		}); err != nil {
			a.fail(w, r, "job", err)
			return
		}
		enqueued++
	}

	a.write(w, r, http.StatusAccepted, map[string]any{
		"candidates": len(ids),
		"enqueued":   enqueued,
	})
}

// enrichStatus reports the enrich backlog: how many held Works still lack their
// artwork or a canonical id, out of the total held — overall and per content
// type. A read, so a person can watch a backfill drain without tailing logs.
func (a *API) enrichStatus(w http.ResponseWriter, r *http.Request) {
	backlog, err := a.catalog.EnrichBacklogCounts(r.Context())
	if err != nil {
		a.fail(w, r, "work", err)
		return
	}
	a.write(w, r, http.StatusOK, map[string]any{
		"under_enriched":  backlog.UnderEnriched,
		"total":           backlog.Total,
		"enriched":        backlog.Total - backlog.UnderEnriched,
		"by_content_type": backlog.ByContentType,
	})
}
