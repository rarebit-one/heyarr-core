package resources

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// Release-candidate evaluation over HTTP (§63).
//
// # Why this endpoint exists at all
//
// §63 says evaluation is deterministic and INSPECTABLE. An evaluation you can
// only reach through a background job is not inspectable — "why did it pick
// that release, last Tuesday, at 3am" cannot be answered by re-running
// something that has since moved on.
//
// So the scorer is reachable directly: hand it candidates and a profile, and
// it answers with the same values the search job (M3-12) will get. That makes
// a profile testable BEFORE it is used in anger, which is the difference
// between tuning a profile in a terminal and tuning it by watching what
// arrives over a week.
//
// It writes nothing. A POST because the body carries the candidates, not
// because it changes anything — the same reasoning as /playback/plan.

// evaluateRequest is the POST body.
type evaluateRequest struct {
	// QualityProfileID or QualityProfile names the standard. Exactly one.
	QualityProfileID string `json:"quality_profile_id"`
	QualityProfile   string `json:"quality_profile"`
	// Candidates are the releases to score.
	Candidates []candidateInput `json:"candidates"`
}

type candidateInput struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Provider string `json:"provider"`
	// Attributes is what is known about the release. A key that is ABSENT
	// means "could not be determined", which is different from a zero value
	// and is reported as `undetermined` rather than as a failure.
	Attributes map[string]policy.Value `json:"attributes"`
}

// evaluateResponse is the ranked answer.
type evaluateResponse struct {
	QualityProfileID string `json:"quality_profile_id"`
	// Ranked is every candidate, best first. Accepted candidates rank above
	// rejected ones; then by score descending; then by id ascending, which is
	// a stable key that does not depend on the order they were supplied in.
	Ranked []rankedCandidate `json:"ranked"`
	// Selected is the id of the candidate that would be acquired, absent when
	// none was acceptable.
	//
	// It is a separate field rather than "the first of Ranked" because the
	// first element of a ranked list is not necessarily acceptable — when
	// everything was rejected it is merely the least bad, and acquiring it
	// would be exactly what §62's gates exist to prevent.
	Selected string `json:"selected,omitempty"`
}

type rankedCandidate struct {
	ID         string               `json:"id"`
	Title      string               `json:"title,omitempty"`
	Provider   string               `json:"provider,omitempty"`
	Accepted   bool                 `json:"accepted"`
	Score      int                  `json:"score"`
	Terminal   bool                 `json:"terminal"`
	Reasons    []acquisition.Reason `json:"reasons"`
	RejectedBy []acquisition.Reason `json:"rejected_by,omitempty"`
}

// maxCandidates bounds one request. A real search returns tens; this is a
// bound on what one write token can make the scorer iterate over, in the same
// spirit as the rule and codec limits elsewhere.
const maxCandidates = 500

// evaluateCandidates is POST /api/v1/quality-profiles/{id}/evaluate.
func (a *API) evaluateCandidates(w http.ResponseWriter, r *http.Request) {
	var body evaluateRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if id := chi.URLParam(r, "id"); id != "" {
		body.QualityProfileID = id
	}
	if len(body.Candidates) == 0 {
		httpapi.Fail(w, r, problem.BadRequest("give me at least one candidate to evaluate"))
		return
	}
	if len(body.Candidates) > maxCandidates {
		httpapi.Fail(w, r, problem.BadRequest(
			fmt.Sprintf("%d candidates is past the limit of %d",
				len(body.Candidates), maxCandidates)))
		return
	}

	stored, err := qualityProfileByID(r.Context(), a.reader, body.QualityProfileID)
	if err != nil {
		a.fail(w, r, "quality profile", err)
		return
	}
	profile := stored.domainProfile()

	candidates := make([]acquisition.ReleaseCandidate, 0, len(body.Candidates))
	seen := make(map[string]bool, len(body.Candidates))
	for i, in := range body.Candidates {
		id := in.ID
		if id == "" {
			httpapi.Fail(w, r, problem.BadRequest(
				fmt.Sprintf("candidate %d has no id — the id is the tie-break, so a "+
					"missing one makes the ranking depend on the order you sent them in", i+1)))
			return
		}
		if seen[id] {
			// Two candidates with one id would make the tie-break ambiguous
			// and the ranking unstable, which is the one property this
			// endpoint promises.
			httpapi.Fail(w, r, problem.BadRequest(
				fmt.Sprintf("two candidates share the id %q", id)))
			return
		}
		seen[id] = true

		attrs, err := parseAttributes(in.Attributes)
		if err != nil {
			httpapi.Fail(w, r, problem.BadRequest(
				fmt.Sprintf("candidate %q: %s", id, err.Error())))
			return
		}
		candidates = append(candidates, acquisition.ReleaseCandidate{
			ID: id, Title: in.Title, Provider: in.Provider, Attributes: attrs,
		})
	}

	ranked := acquisition.EvaluateAll(candidates, profile)
	out := evaluateResponse{
		QualityProfileID: stored.ID,
		Ranked:           make([]rankedCandidate, 0, len(ranked)),
	}
	for _, r := range ranked {
		out.Ranked = append(out.Ranked, rankedCandidate{
			ID:         r.Candidate.ID,
			Title:      r.Candidate.Title,
			Provider:   r.Candidate.Provider,
			Accepted:   r.Evaluation.Accepted,
			Score:      r.Evaluation.Score,
			Terminal:   r.Evaluation.Terminal,
			Reasons:    r.Evaluation.Reasons,
			RejectedBy: r.Evaluation.RejectedBy(),
		})
	}
	if best, ok := acquisition.Best(ranked); ok {
		out.Selected = best.Candidate.ID
	}
	a.write(w, r, http.StatusOK, out)
}

// parseAttributes validates the attribute map, refusing an unknown attribute
// by name.
//
// The same vocabulary check the profile writer gets (M3-01), applied to the
// other side: an attribute nothing recognises is a typo, and silently ignoring
// it would produce an evaluation that looks right and scored against nothing.
func parseAttributes(in map[string]policy.Value) (acquisition.Attributes, error) {
	out := make(acquisition.Attributes, len(in))
	for name, value := range in {
		attr := policy.Attribute(name)
		kind, known := policy.KindOf(attr)
		if !known {
			return nil, fmt.Errorf("there is no attribute called %q", name)
		}
		if !value.IsSet() {
			// An explicit null means "could not determine", which is the same
			// as leaving the key out. Accepting both spellings is kinder than
			// making a provider remember which one Heyarr wanted.
			continue
		}
		if value.Kind != kind {
			return nil, fmt.Errorf("%s is a %s attribute and was given %s",
				name, kind, value.String())
		}
		out[attr] = value
	}
	return out, nil
}
