//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// fakeCatalogSync is a scripted on-demand convergence trigger.
type fakeCatalogSync struct {
	synced, deferred int
	err              error
	calls            int
}

func (f *fakeCatalogSync) SyncAll(context.Context) (int, int, error) {
	f.calls++
	return f.synced, f.deferred, f.err
}

// A node not wired for two-site convergence answers 503, not a broken 200.
func TestCatalogSyncRouteAnswers503WhenNotWired(t *testing.T) {
	h := newHarness(t)
	resp := h.doStable(http.MethodPost, "/api/v1/catalog/sync", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("catalog/sync with no sibling = %d, want 503: %s", resp.StatusCode, h.body(resp))
	}
}

// When wired, the route runs the pass and reports what converged.
func TestCatalogSyncRouteRunsThePassAndReportsCounts(t *testing.T) {
	trigger := &fakeCatalogSync{synced: 1, deferred: 0}
	h := newHarness(t, withCatalogSyncTrigger(trigger))

	resp := h.doStable(http.MethodPost, "/api/v1/catalog/sync", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catalog/sync = %d, want 200: %s", resp.StatusCode, h.body(resp))
	}
	if trigger.calls != 1 {
		t.Errorf("the route drove the syncer %d times, want 1", trigger.calls)
	}
	var got struct {
		Synced   int `json:"synced"`
		Deferred int `json:"deferred"`
	}
	if err := json.Unmarshal([]byte(h.body(resp)), &got); err != nil {
		t.Fatalf("sync response is not decodable: %v", err)
	}
	if got.Synced != 1 || got.Deferred != 0 {
		t.Errorf("reported (synced=%d, deferred=%d), want (1,0)", got.Synced, got.Deferred)
	}
}

// A pass that fails locally is a 500, not a partial success.
func TestCatalogSyncRouteFailsClosedOnAStoreError(t *testing.T) {
	trigger := &fakeCatalogSync{err: errors.New("db gone")}
	h := newHarness(t, withCatalogSyncTrigger(trigger))
	if got := h.doStable(http.MethodPost, "/api/v1/catalog/sync", nil).StatusCode; got != http.StatusInternalServerError {
		t.Fatalf("a failed pass = %d, want 500", got)
	}
}
