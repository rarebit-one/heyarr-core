// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The enrich backfill and status reads over held films (ADR-0087, ADR-0122). The
// seed holds work1, a movie with a primary asset and no artwork.

func enrichHarness(t *testing.T) *harness {
	t.Helper()
	reg := providers.New(nil)
	if err := reg.Register(providers.NewFake("fake-tmdb", providers.CapabilityEnrich)); err != nil {
		t.Fatal(err)
	}
	return newHarness(t, withProviders(reg)).seed()
}

func TestEnrichBackfillScopesByContentType(t *testing.T) {
	t.Parallel()
	h := enrichHarness(t)

	resp := h.doStable(http.MethodPost, "/api/v1/enrich/backfill", strings.NewReader(`{"all":true,"content_type":"movie"}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("backfill = %d: %s", resp.StatusCode, h.body(resp))
	}
	var got struct{ Candidates, Enqueued int }
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	if got.Candidates == 0 || got.Enqueued != got.Candidates {
		t.Fatalf("backfill = %+v, want every held film queued", got)
	}
	if n := h.countRows(t, `SELECT count(*) FROM jobs WHERE type = 'enrich_work'`); n != got.Enqueued {
		t.Fatalf("%d enrich jobs in the table, want %d", n, got.Enqueued)
	}
	// Every queued job is for a movie: the scope held.
	if n := h.countRows(t, `SELECT count(*) FROM jobs j JOIN works w ON j.payload ->> 'work_id' = w.id
		WHERE j.type = 'enrich_work' AND w.content_type <> 'movie'`); n != 0 {
		t.Fatalf("%d jobs for non-movie works leaked past the scope", n)
	}

	// A re-run dedupes on the work-scoped key: no second live job per work.
	resp = h.doStable(http.MethodPost, "/api/v1/enrich/backfill", strings.NewReader(`{"all":true,"content_type":"movie"}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("second backfill = %d", resp.StatusCode)
	}
	if n := h.countRows(t, `SELECT count(*) FROM jobs WHERE type = 'enrich_work'`); n != got.Enqueued {
		t.Fatalf("%d enrich jobs after a re-run, want still %d", n, got.Enqueued)
	}

	// A typo is refused, not silently matched to nothing.
	resp = h.doStable(http.MethodPost, "/api/v1/enrich/backfill", strings.NewReader(`{"all":true,"content_type":"movies"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bad content_type = %d, want 400", resp.StatusCode)
	}
	if body := string(h.body(resp)); !strings.Contains(body, "content_type must be one of") {
		t.Fatalf("the refusal should name the set: %s", body)
	}
}

func TestEnrichStatusBreaksDownByContentType(t *testing.T) {
	t.Parallel()
	h := enrichHarness(t)
	var got struct {
		UnderEnriched int `json:"under_enriched"`
		Total         int `json:"total"`
		Enriched      int `json:"enriched"`
		ByContentType map[string]struct {
			UnderEnriched int `json:"under_enriched"`
			Total         int `json:"total"`
		} `json:"by_content_type"`
	}
	if err := json.Unmarshal(h.body(h.get("/api/v1/enrich/status")), &got); err != nil {
		t.Fatal(err)
	}
	movies, ok := got.ByContentType["movie"]
	if !ok || movies.Total == 0 || movies.UnderEnriched != movies.Total {
		t.Fatalf("status = %+v, want the held films counted as under-enriched", got)
	}
	if got.Total != got.UnderEnriched+got.Enriched {
		t.Fatalf("status = %+v: the totals do not add up", got)
	}
}
