// The continue rail (ADR-0075): the newest unfinished session per work, with
// a position, folded so a work someone paused three times is one card.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

const (
	stoppedSessionID = "01990000-0000-7000-8000-0000000000s3"
	freshSessionID   = "01990000-0000-7000-8000-0000000000s4"
	trackSessionID   = "01990000-0000-7000-8000-0000000000s5"
)

// seedContinue adds, on top of seed()'s paused film (s1, device1) and completed
// book (s2, device2): a NEWER stopped session on the same film from the same
// device, a created session on a second film that never recorded a position,
// and a paused listen on a track from the other device.
func (h *harness) seedContinue() *harness {
	h.t.Helper()
	h.exec(`INSERT INTO consumption_sessions
		(id, asset_id, device_id, verb, state, progress_locator, progress_unit,
		 created_at, updated_at, started_at, ended_at) VALUES
		(?, ?, ?, 'watch', 'stopped', '2000', 'seconds', ?, '2026-08-02T00:00:00Z', ?, '2026-08-02T00:00:00Z'),
		(?, ?, ?, 'watch', 'created', '', '', ?, '2026-08-03T00:00:00Z', NULL, NULL),
		(?, ?, ?, 'listen', 'paused', '61.25', 'seconds', ?, '2026-08-04T00:00:00Z', ?, NULL)`,
		stoppedSessionID, asset1ID, device1ID, seedTime, seedTime,
		freshSessionID, asset2ID, device1ID, seedTime,
		trackSessionID, track1AssetID, device2ID, seedTime, seedTime)
	return h
}

type continueOut struct {
	Items []struct {
		Session struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"session"`
		Work struct {
			ID      string          `json:"id"`
			Artwork json.RawMessage `json:"artwork"`
		} `json:"work"`
		Asset struct {
			AssetID         string   `json:"asset_id"`
			DurationSeconds *float64 `json:"duration_seconds"`
		} `json:"asset"`
	} `json:"items"`
}

func (h *harness) continueRail(t *testing.T, query string) continueOut {
	t.Helper()
	resp := h.get("/api/v1/consumption/continue" + query)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	var out continueOut
	if err := json.Unmarshal(h.body(resp), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestContinueShape(t *testing.T) {
	h := newHarness(t).seed().seedBrowse().seedContinue()
	resp := h.doStable(http.MethodGet, "/api/v1/consumption/continue", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	testutil.Golden(t, goldenPath("continue.json"), h.indent(resp))
}

func TestContinueFoldsToTheNewestSessionPerWork(t *testing.T) {
	h := newHarness(t).seed().seedBrowse().seedContinue()
	out := h.continueRail(t, "")

	// Two works have a position: the film (three sessions, one wins) and the
	// track. The book's session is completed; the second film's never recorded.
	if len(out.Items) != 2 {
		t.Fatalf("items = %+v, want two", out.Items)
	}
	track, film := out.Items[0], out.Items[1]
	if track.Session.ID != trackSessionID || track.Work.ID != work4ID {
		t.Errorf("newest first: %+v", track)
	}
	if film.Session.ID != stoppedSessionID || film.Session.State != "stopped" {
		t.Errorf("the film should resume from its newest (stopped) session, got %+v", film.Session)
	}
	if film.Work.ID != work1ID || film.Asset.AssetID != asset1ID {
		t.Errorf("film entry = %+v", film)
	}
	if film.Asset.DurationSeconds == nil || *film.Asset.DurationSeconds != 6960.5 {
		t.Errorf("duration = %v, want the probed one", film.Asset.DurationSeconds)
	}
	if !strings.Contains(string(film.Work.Artwork), posterAssetID) {
		t.Errorf("the film card carries its poster: %s", film.Work.Artwork)
	}
}

func TestContinueFilters(t *testing.T) {
	h := newHarness(t).seed().seedBrowse().seedContinue()

	if out := h.continueRail(t, "?device_id="+device1ID); len(out.Items) != 1 || out.Items[0].Work.ID != work1ID {
		t.Fatalf("device filter: %+v", out.Items)
	}
	if out := h.continueRail(t, "?limit=1"); len(out.Items) != 1 || out.Items[0].Work.ID != work4ID {
		t.Fatalf("limit: %+v", out.Items)
	}
	if resp := h.get("/api/v1/consumption/continue?limit=0"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("limit=0 = %d, want 400", resp.StatusCode)
	}
	if resp := h.get("/api/v1/consumption/continue?limit=x"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("limit=x = %d, want 400", resp.StatusCode)
	}
}

func TestContinueIsClosedToAGuest(t *testing.T) {
	h := newHarness(t, withAuth, withGuest).seed().seedBrowse().seedContinue()
	if resp := h.get("/api/v1/consumption/continue"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("guest = %d, want 403", resp.StatusCode)
	}
	tok := h.mint("reader", auth.ScopeRead)
	if resp := h.do(http.MethodGet, "/api/v1/consumption/continue", tok.Secret, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("reader = %d, want 200", resp.StatusCode)
	}
}
