package resources

import (
	"context"
	"net/http"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// CatalogSyncTrigger runs one on-demand pass of two-site catalog convergence:
// it exchanges the editorial op-log with each sibling so a delete made here
// reaches the other site now, rather than on the next beat (ADR-0073, #449).
// *catalogsync.Syncer satisfies it. It is the caller behind POST
// /api/v1/catalog/sync — the force-sync an operator (or the demo) reaches for.
//
// nil is legitimate: a single-site node with no peer surface converges with
// nobody, and the route answers 503 rather than 500.
type CatalogSyncTrigger interface {
	SyncAll(ctx context.Context) (synced, deferred int, err error)
}

// catalogSyncResult acks an on-demand convergence pass: how many siblings
// converged and how many were deferred (unreachable this pass, ADR-0038).
type catalogSyncResult struct {
	Synced   int `json:"synced"`
	Deferred int `json:"deferred"`
}

// syncCatalog runs one catalog-ops convergence pass on demand. It is the same
// exchange the scheduled beat runs, exposed so an operator can force it and the
// acceptance demo can assert convergence without waiting on a cadence — the
// mirror of POST /api/v1/state/replicate for the catalog plane.
func (a *API) syncCatalog(w http.ResponseWriter, r *http.Request) {
	if a.catalogSync == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node converges a catalog with no sibling; catalog ops "+
				"flow between the two peers of a two-site pair (§49, ADR-0073)"))
		return
	}
	synced, deferred, err := a.catalogSync.SyncAll(r.Context())
	if err != nil {
		a.log.Error("on-demand catalog-ops sync failed",
			"request_id", httpapi.RequestIDFrom(r.Context()), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	a.log.Info("converged the catalog op-log with siblings", "synced", synced, "deferred", deferred)
	a.write(w, r, http.StatusOK, catalogSyncResult{Synced: synced, Deferred: deferred})
}
