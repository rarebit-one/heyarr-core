package resources

import (
	"context"

	"github.com/rarebit-one/heyarr-core/internal/domain/renderroute"
)

// AllowLoopbackReports lets a test harness report fake renderers served by
// httptest on 127.0.0.1, which the production SSRF guard refuses.
func AllowLoopbackReports(a *API) { a.allowLoopbackReports = true }

// SiblingAssets exposes the smaller-copy lookup the route planner's refusal
// draws its alternatives from (ADR-0111 §R2).
func SiblingAssets(a *API, ctx context.Context, assetID string) []renderroute.SiblingAsset {
	return a.siblingAssets(ctx, assetID)
}
