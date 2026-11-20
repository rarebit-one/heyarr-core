package resources

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// FanOutOp is one sibling's acquire op and what this site did with it
// (ADR-0119, #799). It answers "why didn't this site get X" for the outcomes
// that never reached a want: scope_unresolved, tombstoned, and a private
// release's no_site_source.
type FanOutOp struct {
	OpHash string `json:"op_hash"`
	// OriginSite is the site that chose the release.
	OriginSite  string `json:"origin_site"`
	ContentType string `json:"content_type"`
	WorkKey     string `json:"work_key"`
	WorkTitle   string `json:"work_title,omitempty"`
	ReleaseName string `json:"release_name"`
	Infohash    string `json:"infohash"`
	Scope       string `json:"scope"`
	Aspect      string `json:"aspect"`
	SelectedAt  string `json:"selected_at"`
	// Outcome is what this site did with the op: applied, held, deferred,
	// tombstoned, scope_unresolved, no_site_source or profile_unknown.
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	// DesiredItemID is the want the op reached here, absent when none.
	DesiredItemID string `json:"desired_item_id,omitempty"`
	AppliedAt     string `json:"applied_at"`
	// ReportedUnsourced is whether this site told the origin it can resolve
	// no source for the release (ADR-0119 #800 amendment).
	ReportedUnsourced bool `json:"reported_unsourced"`
}

// fanOutOutcomes are the outcomes a sibling's op can have here. `own` is
// this site's own op, which is not fan-out.
var fanOutOutcomes = []string{
	string(catalog.FanOutApplied), string(catalog.FanOutHeld), string(catalog.FanOutDeferred),
	string(catalog.FanOutTombstoned), string(catalog.FanOutScopeUnresolved),
	string(catalog.FanOutNoSiteSource), string(catalog.FanOutProfileUnknown),
}

// ListFanOut lists the siblings' acquire ops this site has an outcome for,
// most recently applied first, optionally one outcome's or one site's. It is
// the read the MCP door and the HTTP route share; limit is bounded by the
// caller.
func (a *API) ListFanOut(ctx context.Context, outcome, site string, limit int) ([]FanOutOp, error) {
	if a.catalog == nil {
		return nil, errors.New("resources: no catalog is wired, so fan-out cannot be read")
	}
	if outcome != "" && !slices.Contains(fanOutOutcomes, outcome) {
		return nil, &badRequest{err: errors.New("outcome must be one of applied, held, deferred, " +
			"tombstoned, scope_unresolved, no_site_source, profile_unknown")}
	}
	found, err := a.catalog.ListFanOut(ctx, catalog.FanOutQuery{
		Outcome: catalog.FanOutOutcome(outcome), Site: site, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]FanOutOp, 0, len(found))
	for _, e := range found {
		out = append(out, FanOutOp{
			OpHash: e.OpHash, OriginSite: e.Site, ContentType: e.ContentType, WorkKey: e.WorkKey,
			WorkTitle: e.WorkTitle, ReleaseName: e.ReleaseTitle, Infohash: e.Infohash,
			Scope: e.Scope, Aspect: e.Aspect, SelectedAt: e.SelectedAt.UTC().Format(time.RFC3339),
			Outcome: string(e.Outcome), Detail: e.Detail, DesiredItemID: e.DesiredItemID,
			AppliedAt: e.AppliedAt.UTC().Format(time.RFC3339), ReportedUnsourced: e.Reported,
		})
	}
	return out, nil
}

// FanOutList is GET /api/v1/fan-out's body. It is not cursor-paged: the ops
// are bounded by the catalog op-log, and truncated says when limit cut it.
type FanOutList struct {
	Items     []FanOutOp `json:"items"`
	Truncated bool       `json:"truncated"`
}

const (
	defaultFanOutLimit = 100
	maxFanOutLimit     = 500
)

// listFanOut is GET /api/v1/fan-out — a shell over ListFanOut.
func (a *API) listFanOut(w http.ResponseWriter, r *http.Request) {
	limit := defaultFanOutLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxFanOutLimit {
			httpapi.Fail(w, r, problem.BadRequest("limit must be an integer from 1 to 500"))
			return
		}
		limit = n
	}
	items, err := a.ListFanOut(r.Context(), r.URL.Query().Get("outcome"), r.URL.Query().Get("origin_site"), limit)
	var bad *badRequest
	switch {
	case errors.As(err, &bad):
		httpapi.Fail(w, r, problem.BadRequest(bad.err.Error()))
		return
	case err != nil:
		a.fail(w, r, "fan-out", err)
		return
	}
	a.write(w, r, http.StatusOK, FanOutList{Items: items, Truncated: len(items) == limit})
}
