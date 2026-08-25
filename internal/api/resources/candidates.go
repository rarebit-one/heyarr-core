package resources

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Release candidates over HTTP (§60, §63, M3-12).
//
// Three operations, and §60 keeps them as three because they are three
// different things a person does:
//
//	POST /desired/{id}/search      run a search NOW and show me what came back
//	GET  /desired/{id}/candidates  what did it find, and why was each judged so
//	POST /desired/{id}/select      take THIS one, whatever the scorer thinks
//
// Manual search and manual override are separate features. A search is the
// same job the beat runs, triggered by a person; an override is a person
// disagreeing with the deterministic scorer, and it must leave a trace or the
// scorer's history stops being reconstructable.
//
// All three are §71 MCP verbs later — search_releases, explain_release,
// acquire_release — so they exist as first-class operations rather than as side
// effects of a background loop.

// CandidateView is one release as offered and as judged.
type CandidateView struct {
	// CandidateID is the PROVIDER's identity for the release, which is what a
	// caller names in a select. The row's own uuid is deliberately not on the
	// wire: it identifies a storage row that a re-search replaces, and a
	// client holding one would be holding a handle to something that vanishes.
	CandidateID string `json:"candidate_id"`
	Provider    string `json:"provider"`
	Title       string `json:"title,omitempty"`

	Accepted bool `json:"accepted"`
	Score    int  `json:"score"`
	Terminal bool `json:"terminal"`
	Selected bool `json:"selected"`

	// Overridden reports that a person chose this against the ranking, and
	// OverrideDetail says what the scorer had said instead.
	Overridden     bool   `json:"overridden,omitempty"`
	OverrideDetail string `json:"override_detail,omitempty"`

	// Reasons is every rule that was considered — §63's deliverable, and the
	// half that answers "why not this one".
	Reasons    []acquisition.Reason `json:"reasons"`
	RejectedBy []acquisition.Reason `json:"rejected_by,omitempty"`
}

// CandidatesResponse is a want's candidates, best first.
type CandidatesResponse struct {
	DesiredItemID string `json:"desired_item_id"`
	// SearchID groups these as the one search's answers they are. A different
	// id from a previous read means a search has happened in between and this
	// is a different set, not an update of the same one.
	SearchID string `json:"search_id,omitempty"`
	// Selected is the provider's id for the chosen release, absent when
	// nothing was acceptable.
	Selected   string          `json:"selected,omitempty"`
	Candidates []CandidateView `json:"candidates"`
}

// selectRequest is the POST /desired/{id}/select body.
type selectRequest struct {
	CandidateID string `json:"candidate_id"`
}

func candidateView(c catalog.Candidate) CandidateView {
	return CandidateView{
		CandidateID:    c.CandidateID,
		Provider:       c.Provider,
		Title:          c.Title,
		Accepted:       c.Evaluation.Accepted,
		Score:          c.Evaluation.Score,
		Terminal:       c.Evaluation.Terminal,
		Selected:       c.Selected,
		Overridden:     c.Overridden,
		OverrideDetail: c.OverrideDetail,
		Reasons:        c.Evaluation.Reasons,
		RejectedBy:     c.Evaluation.RejectedBy(),
	}
}

// listCandidates is GET /api/v1/desired/{id}/candidates.
//
// It reads what the last search stored rather than re-searching. §63's
// inspectability is about what WAS decided, and re-running the scorer here
// would answer a different question — what would be decided now — which is
// exactly the substitution that makes an audit trail worthless.
func (a *API) listCandidates(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := desiredByID(r.Context(), a.reader, id); err != nil {
		a.fail(w, r, "desired item", err)
		return
	}

	rows, err := a.catalog.CandidatesFor(r.Context(), id)
	if err != nil {
		a.fail(w, r, "candidate", err)
		return
	}

	out := CandidatesResponse{
		DesiredItemID: id,
		Candidates:    make([]CandidateView, 0, len(rows)),
	}
	for _, c := range rows {
		if out.SearchID == "" {
			out.SearchID = c.SearchID
		}
		if c.Selected {
			out.Selected = c.CandidateID
		}
		out.Candidates = append(out.Candidates, candidateView(c))
	}
	a.write(w, r, http.StatusOK, out)
}

// searchDesired is POST /api/v1/desired/{id}/search.
//
// Enqueues the job rather than running it. A search is a job (invariant 4,
// ADR-0002) and the worker that runs it may be another process, so this
// answers 202 rather than pretending to be the worker — and rather than
// holding an HTTP request open across a provider round trip that an indexer
// may take thirty seconds to refuse.
func (a *API) searchDesired(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := desiredByID(r.Context(), a.reader, id); err != nil {
		a.fail(w, r, "desired item", err)
		return
	}
	out, err := a.SearchReleases(r.Context(), id)
	if err != nil {
		a.fail(w, r, "job", err)
		return
	}
	a.write(w, r, http.StatusAccepted, out)
}

// SearchReleases queues a search for one want and says so.
//
// Exported because MCP's search_releases is the same action asked for by an
// agent instead of by HTTP, and §71's vocabulary should not be a second
// implementation of it — a second one is how the two come to disagree about
// what a search does. The HTTP handler above is now a thin wrapper.
//
// It ENQUEUES. A search is a job (invariant 4, ADR-0002), the worker that runs
// it may be another process, and an indexer may take thirty seconds to refuse
// — so the answer is "queued", not the candidates. A caller that wants the
// result reads the want's candidates afterwards.
func (a *API) SearchReleases(ctx context.Context, id string) (map[string]string, error) {
	job, err := a.jobs.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      acquisition.SearchJobType,
		Payload:   acquisition.SearchPayload{DesiredItemID: id},
		DedupeKey: acquisition.SearchDedupeKey(id),
	})
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"desired_item_id": id,
		"job_id":          job.ID,
		"status":          "queued",
	}, nil
}

// selectCandidate is POST /api/v1/desired/{id}/select — §60's manual override.
//
// It refuses a candidate the profile rejected, and that refusal is deliberate.
// The gates in §62 are the operator's own statement of what is acceptable; an
// override that could ignore them would quietly turn "accept" into a
// suggestion. Wanting something outside the profile is perfectly expressible —
// by changing the profile, which is visible and durable, rather than by a
// one-off that leaves the profile saying something nobody means.
func (a *API) selectCandidate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := desiredByID(r.Context(), a.reader, id); err != nil {
		a.fail(w, r, "desired item", err)
		return
	}
	var body selectRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("candidate_id", body.CandidateID); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	chosen, err := a.AcquireRelease(r.Context(), id, body.CandidateID)
	switch {
	case errors.Is(err, catalog.ErrNoCandidate):
		httpapi.Fail(w, r, problem.NotFound(
			"no candidate with that id for this want — it may have been superseded by a later search"))
		return
	case errors.Is(err, catalog.ErrNotAcceptable):
		httpapi.Fail(w, r, problem.Conflict(
			"that candidate was rejected by the quality profile — "+
				"change the profile if it should be acceptable, rather than overriding it here"))
		return
	case err != nil:
		a.fail(w, r, "candidate", err)
		return
	}
	a.write(w, r, http.StatusOK, candidateView(chosen))
}

// AcquireRelease selects one candidate for a want and arranges for it to be
// fetched — §60's manual override, and MCP's acquire_release.
//
// Exported for the same reason SearchReleases is: the agent vocabulary and the
// HTTP surface are the same ACTION reached two ways, and two implementations
// would eventually disagree about whether selecting also grabs.
//
// It refuses a candidate the profile rejected, and that refusal is the point.
// §62's gates are the operator's own statement of what is acceptable; an
// override that could ignore them would quietly turn "accept" into a
// suggestion. Wanting something outside the profile is expressible by changing
// the profile — which is visible and durable — rather than by a one-off that
// leaves the profile saying something nobody means.
func (a *API) AcquireRelease(
	ctx context.Context, id, candidateID string,
) (catalog.Candidate, error) {
	chosen, err := a.catalog.OverrideSelection(ctx, id, candidateID)
	if err != nil {
		return catalog.Candidate{}, err
	}

	// The state machine only moves if the want was not already SELECTED. An
	// override during CANDIDATES_FOUND advances it; one that merely re-points
	// an existing selection changes the row and not the phase.
	if _, err := a.catalog.AdvanceAcquisition(ctx, id,
		acquisition.TransitionSelect, "selected by hand"); err != nil {
		if !errors.Is(err, acquisition.ErrIllegalTransition) {
			return catalog.Candidate{}, err
		}
	}

	// And queue the grab, exactly as the search beat does (#225).
	//
	// Both routes to SELECTED enqueue this, because a want chosen by hand is
	// no more able to fetch itself than one chosen by the scorer — and wiring
	// only the automatic route would have left §60's manual override as the
	// one path that still dead-ends, which is the harder half to notice.
	//
	// Enqueue failure is logged rather than returned: the override itself is
	// durable and is what the caller asked for. The want stays SELECTED and the
	// next search re-enqueues.
	if _, err := a.jobs.Enqueue(ctx, jobs.EnqueueOptions{
		Type: acquisition.GrabJobType,
		Payload: acquisition.GrabPayload{
			DesiredItemID: id,
			CandidateID:   chosen.CandidateID,
		},
		DedupeKey:          acquisition.GrabDedupeKey(id),
		RequiredCapability: providers.CapabilityDownload.JobCapability(),
	}); err != nil {
		a.log.Warn("could not queue the grab for a hand-selected release",
			"desired_item_id", id, "candidate", chosen.CandidateID, "error", err)
	}
	return chosen, nil
}
