package resources

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// Adopting a completed acquisition (§65, M3-13).
//
// # Why this endpoint exists, and why it is not test scaffolding
//
// §65 lists many ways bytes arrive, and "completed acquisition" is only the
// first. The ordinary path is a download client Heyarr polls — but a client it
// does NOT poll is an ordinary situation too: a transfer finished before
// Heyarr knew about it, an operator fetched something by hand, a client the
// registry has no integration for.
//
// This says "these bytes are here, they answer this want, take them". The
// pipeline afterwards is identical to the polled path — the same verification,
// the same ingest job, the same §64 edges — because the only thing that
// differs is who noticed the transfer finished.
//
// It is deliberately NOT an upload: Heyarr does not receive the bytes, it is
// told where they already are. An upload is a different §65 source with a
// different shape, and conflating them would put a multi-gigabyte body on an
// endpoint whose whole job is a path.

// adoptAcquisitionRequest is the POST body.
type adoptAcquisitionRequest struct {
	// Provider names what produced the bytes. Free text: this endpoint exists
	// precisely for clients the registry does not know about.
	Provider string `json:"provider"`
	// ExternalID is that producer's identifier — an infohash, a job id.
	// Required, because it is what makes adopting the same transfer twice
	// idempotent rather than duplicative.
	ExternalID   string `json:"external_id"`
	ExternalName string `json:"external_name"`
	// LocalPath is where the bytes are, in a namespace HEYARR can open. Not
	// the client's namespace — path mapping is the poller's job and there is
	// no client here to map from.
	LocalPath string `json:"local_path"`
}

// adoptAcquisition is POST /api/v1/desired/{id}/acquisitions.
func (a *API) adoptAcquisition(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := desiredByID(r.Context(), a.reader, id); err != nil {
		a.fail(w, r, "desired item", err)
		return
	}

	var body adoptAcquisitionRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	for _, f := range []struct{ name, value string }{
		{"provider", body.Provider},
		{"external_id", body.ExternalID},
		{"local_path", body.LocalPath},
	} {
		if strings.TrimSpace(f.value) == "" {
			httpapi.Fail(w, r, problem.BadRequest(f.name+" is required"))
			return
		}
	}

	// The want must be somewhere the pipeline can leave from. Adopting into a
	// want that is mid-search would mean two things racing to decide what it
	// is acquiring.
	state, err := a.catalog.Acquisition(r.Context(), id)
	if err != nil {
		a.fail(w, r, "desired item", err)
		return
	}

	if _, err := a.catalog.RecordAcquisition(r.Context(), catalog.Acquisition{
		ID:            a.newID(),
		DesiredItemID: id,
		Provider:      body.Provider,
		ExternalID:    body.ExternalID,
		ExternalName:  body.ExternalName,
		RemotePath:    body.LocalPath,
		LocalPath:     body.LocalPath,
	}); err != nil {
		if isUniqueViolation(err) {
			httpapi.Fail(w, r, problem.Conflict(
				"that want already has an acquisition in flight"))
			return
		}
		a.fail(w, r, "acquisition", err)
		return
	}

	// Walk to VERIFYING through the edges the polled path takes, rather than
	// jumping there. §64's machine refuses a skip, and it should: a want that
	// arrived at VERIFYING without passing through QUEUED has a history that
	// does not describe what happened.
	//
	// A want already past these is left where it is — adopting the same
	// transfer twice is the normal case for a retried request.
	//
	// A want that never STARTED, though, cannot walk them: the only forward
	// edge from idle is `search`, so every one of these is refused and the
	// want stays exactly where it was — while the request answered 202 and the
	// ingest job went on to succeed having done nothing (#240).
	//
	// That is the case this endpoint documents itself as existing for:
	// "something fetched by hand" is precisely a want nobody searched. So it
	// adopts, on the edge that models bytes arriving from outside the pipeline
	// rather than on a fabricated history of a search that never ran.
	walk := []acquisition.Transition{
		acquisition.TransitionQueue,
		acquisition.TransitionStartDownload,
		acquisition.TransitionDownloaded,
	}
	if _, err := state.State.Apply(acquisition.TransitionAdopt); err == nil {
		walk = []acquisition.Transition{acquisition.TransitionAdopt}
	}
	for _, tr := range walk {
		if _, err := state.State.Apply(tr); err != nil {
			continue
		}
		rec, err := a.catalog.AdvanceAcquisition(r.Context(), id, tr, "adopted")
		if err != nil {
			a.fail(w, r, "acquisition", err)
			return
		}
		state = rec
	}

	// The ingest itself is a JOB (invariant 4): hashing and materialising a
	// large file is work, and the worker that does it may be another process.
	job, err := a.jobs.Enqueue(r.Context(), jobs.EnqueueOptions{
		Type:      acquisition.IngestJobType,
		Payload:   acquisition.IngestPayload{DesiredItemID: id},
		DedupeKey: acquisition.IngestDedupeKey(id),
	})
	if err != nil {
		a.fail(w, r, "job", err)
		return
	}

	a.write(w, r, http.StatusAccepted, map[string]string{
		"desired_item_id": id,
		"job_id":          job.ID,
		"phase":           string(state.State.Phase),
		"status":          "queued",
	})
}
