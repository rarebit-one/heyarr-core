package resources

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
)

// Why a want is or is not satisfied (§56, §57).
//
// The acquisition state on a DesiredItem says WHAT the answer is. This says
// WHY, and it is the endpoint that makes "I have this film, why does Heyarr
// say it is missing" answerable — which is the question this axis will be
// asked most often, and the one an operator currently has no way to ask.
//
// It is per want rather than inlined on the listing because a page of fifty
// wants would carry fifty evaluations of several rules each, and the question
// is asked about one want at a time.

// SatisfactionResponse explains both of §56's axes.
type SatisfactionResponse struct {
	DesiredItemID string `json:"desired_item_id"`
	// State is §64's derived name, so this response stands alone.
	State string `json:"state"`

	Content   ContentSatisfaction   `json:"content"`
	Placement PlacementSatisfaction `json:"placement"`
	// Upgrade answers "could this be better" (§60), which is a different
	// question from either axis: a want can be fully satisfied and still
	// improvable, and that gap is the whole upgrade workflow.
	Upgrade UpgradeEligibility `json:"upgrade"`
}

// UpgradeEligibility is whether a want could be improved, and why not when it
// could not (§60, M3-06).
type UpgradeEligibility struct {
	// Eligible is whether nothing about this want's state rules an upgrade
	// out: monitored, satisfied, and not yet terminal.
	Eligible bool `json:"eligible"`
	// Status is WHY, and it is an enumerated reason rather than a bare
	// boolean because "no upgrade" has four completely different meanings —
	// not monitored, not satisfied, already terminal, or simply nothing
	// better on offer. An operator asking "why is this not upgrading" needs
	// to know which.
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// ContentSatisfaction is "do we hold bytes the profile accepts", and why.
type ContentSatisfaction struct {
	Satisfaction string `json:"satisfaction"`
	// SatisfiedBy is the asset that satisfies, when one does — the BEST one,
	// by the same ranking the search uses, so "what am I watching" and "what
	// would I have acquired" cannot disagree.
	SatisfiedBy string `json:"satisfied_by,omitempty"`
	// Assets is every asset considered, with its full §63 reasons. An asset
	// that exists and was rejected appears here with the rule that rejected
	// it, which is the whole point of the endpoint.
	Assets []AssetSatisfaction `json:"assets"`
}

// AssetSatisfaction is one asset scored against the profile.
type AssetSatisfaction struct {
	AssetID    string               `json:"asset_id"`
	Accepted   bool                 `json:"accepted"`
	Score      int                  `json:"score"`
	Terminal   bool                 `json:"terminal"`
	Reasons    []acquisition.Reason `json:"reasons"`
	RejectedBy []acquisition.Reason `json:"rejected_by,omitempty"`
}

// PlacementSatisfaction is "are those bytes everywhere they should be", and why.
type PlacementSatisfaction struct {
	Satisfaction string `json:"satisfaction"`
	// Missing names the peers that should hold the bytes and do not.
	// "Converging" with no list of what is missing is a status nobody can act
	// on.
	Missing []string `json:"missing,omitempty"`
	Detail  string   `json:"detail"`
	// Unproven says plainly that this axis has never run against a second
	// peer (ADR-0010).
	//
	// It is a field rather than only a doc comment because a caveat that lives
	// in the OpenAPI is a caveat the person reading the response does not see.
	// With one peer in the target set, placement is satisfied the moment
	// content is, and `converging` is unreachable — so a client must not read
	// `satisfied` here as evidence that replication works.
	Unproven bool `json:"unproven"`
}

// Satisfaction explains one want: both of §56's axes, plus whether it could be
// better (§60).
//
// Exported for the same reason as WantContent — MCP's get_content_satisfaction
// asks exactly this question, and a second implementation would eventually
// answer it differently. It reconciles rather than reading a cached answer,
// because an explanation that might be minutes stale is one nobody can trust
// while looking at a file they can see on disk.
func (a *API) Satisfaction(ctx context.Context, id string) (SatisfactionResponse, error) {
	if a.catalog == nil {
		return SatisfactionResponse{}, errors.New("resources: no catalog is wired, so " +
			"satisfaction cannot be evaluated")
	}
	want, err := desiredByID(ctx, a.reader, id)
	if err != nil {
		return SatisfactionResponse{}, err
	}

	result, err := a.catalog.ReconcileDesired(ctx, id)
	if err != nil {
		return SatisfactionResponse{}, err
	}

	out := SatisfactionResponse{
		DesiredItemID: id,
		State:         result.State.Name(),
		Content: ContentSatisfaction{
			Satisfaction: string(result.Content.Satisfaction),
			SatisfiedBy:  result.Content.SatisfiedBy,
			Assets:       make([]AssetSatisfaction, 0, len(result.Content.Evaluations)),
		},
		Placement: PlacementSatisfaction{
			Satisfaction: string(result.Placement.Satisfaction),
			Missing:      result.Placement.Missing,
			Detail:       result.Placement.Detail,
			Unproven:     true,
		},
	}
	var incumbent acquisition.Evaluation
	for _, e := range result.Content.Evaluations {
		out.Content.Assets = append(out.Content.Assets, AssetSatisfaction{
			AssetID:    e.AssetID,
			Accepted:   e.Evaluation.Accepted,
			Score:      e.Evaluation.Score,
			Terminal:   e.Evaluation.Terminal,
			Reasons:    e.Evaluation.Reasons,
			RejectedBy: e.Evaluation.RejectedBy(),
		})
		if e.AssetID == result.Content.SatisfiedBy {
			incumbent = e.Evaluation
		}
	}

	// The upgrade question, answered from the SAME evaluation reconciliation
	// just produced. Re-scoring the incumbent here would be a second opinion
	// about the same bytes under the same profile, which is exactly the drift
	// §60 warns about.
	satisfied := result.Content.Satisfaction == acquisition.SatisfactionSatisfied
	verdict := acquisition.UpgradableVerdict(want.Monitor, satisfied, incumbent)
	out.Upgrade = UpgradeEligibility{
		Eligible: acquisition.Eligible(want.Monitor, satisfied, incumbent),
		Status:   string(verdict.Status),
		Detail:   verdict.Detail,
	}
	return out, nil
}

// getSatisfaction is GET /api/v1/desired/{id}/satisfaction — a shell over
// Satisfaction.
func (a *API) getSatisfaction(w http.ResponseWriter, r *http.Request) {
	out, err := a.Satisfaction(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		a.fail(w, r, "desired item", err)
		return
	}
	a.write(w, r, http.StatusOK, out)
}

// reconcileDesired is POST /api/v1/desired/{id}/reconcile.
//
// Enqueues the job rather than running it. Reconciling is a job (invariant 4,
// ADR-0002), and the worker that runs it may be another process — so this
// endpoint says "please look at this" and answers 202, rather than pretending
// to be the worker.
func (a *API) reconcileDesired(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := desiredByID(r.Context(), a.reader, id); err != nil {
		a.fail(w, r, "desired item", err)
		return
	}
	job, err := a.jobs.Enqueue(r.Context(), jobs.EnqueueOptions{
		Type:      acquisition.ReconcileJobType,
		Payload:   acquisition.ReconcilePayload{DesiredItemID: id},
		DedupeKey: acquisition.ReconcileDedupeKey + ":" + id,
	})
	if err != nil {
		a.fail(w, r, "job", err)
		return
	}
	a.write(w, r, http.StatusAccepted, map[string]string{
		"desired_item_id": id,
		"job_id":          job.ID,
		"status":          "queued",
	})
}
