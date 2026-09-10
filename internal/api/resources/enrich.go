package resources

import (
	"net/http"

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
// node is a large batch of jobs against keyless public services, and "I meant
// this one author" should not be one forgotten flag from "every book you have".
type EnrichBackfillRequest struct {
	LibraryID string `json:"library_id"`
	WorkID    string `json:"work_id"`
	Author    string `json:"author"`
	All       bool   `json:"all"`
}

// backfillEnrich enqueues an enrich_work job for every held music/book Work in
// scope that is under-enriched (no cover and/or no canonical id), ignoring the
// beat's backoff schedule (ADR-0087). The beat enriches on its own cadence; this
// is the operator's lever to enrich a scope now — after wiring an enrich provider,
// or after ingesting a shelf of books.
//
// "Under-enriched" is the filter on purpose: a Work that already has its cover and
// id is left alone, so a re-run is cheap and does not re-hit the services for
// nothing. The enrich job is itself idempotent, so a Work enqueued twice (a
// concurrent beat pass) dedupes on its work-scoped key.
func (a *API) backfillEnrich(w http.ResponseWriter, r *http.Request) {
	var body EnrichBackfillRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if body.LibraryID == "" && body.WorkID == "" && body.Author == "" && !body.All {
		httpapi.Fail(w, r, problem.BadRequest(
			"a scope is required: library_id, work_id, author, or all=true — refusing to guess between one author and the whole node"))
		return
	}

	// A node with no enrich provider would enqueue jobs that stay PENDING forever
	// (ADR-0025). That is a legitimate state on a controller-only node, but for an
	// on-demand backfill it is almost always a misconfiguration the operator wants
	// told about rather than silently queued.
	if !a.providers.Has(providers.CapabilityEnrich) {
		httpapi.Fail(w, r, problem.BadRequest(
			"no enrich provider is configured on this node — enrich jobs would never run; configure musicbrainz or openlibrary first"))
		return
	}

	ids, err := a.catalog.WorksNeedingEnrich(r.Context(), catalog.EnrichScope{
		LibraryID: body.LibraryID, WorkID: body.WorkID, Author: body.Author, All: body.All,
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

// enrichStatus reports the enrich backlog: how many held music/book Works still
// lack a cover or a canonical id, out of the total held. A read, so a person can
// watch a backfill drain without tailing logs.
func (a *API) enrichStatus(w http.ResponseWriter, r *http.Request) {
	bare, total, err := a.catalog.EnrichBacklog(r.Context())
	if err != nil {
		a.fail(w, r, "work", err)
		return
	}
	a.write(w, r, http.StatusOK, map[string]any{
		"under_enriched": bare,
		"total":          total,
		"enriched":       total - bare,
	})
}
