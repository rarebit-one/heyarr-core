//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A by-hand select or block on a want whose grab is already at the client
// (#888), over HTTP. The worker tests drive the fake download client through
// the same rows; these prove what the caller is told and what is queued.

const inFlightTransfer = "481b6e3617be4c88f96cb25e47c9d8272130071e"

// queueOnGood puts the seeded want QUEUED (or DOWNLOADING) on a transfer of
// its selected release, "good", having fetched bytesDone.
func (h *harness) queueOnGood(t *testing.T, phase string, bytesDone int) {
	t.Helper()
	h.seedCandidates(t)
	h.exec(`UPDATE acquisition_state SET phase = ? WHERE desired_item_id = ?`, phase, desired1ID)
	h.exec(`INSERT INTO acquisitions
		(id, desired_item_id, provider, external_id, external_name, remote_path, local_path,
		 bytes_total, bytes_done, trouble, created_at, updated_at, last_seen_at)
		VALUES ('01990000-0000-7000-8000-0000000000q1', ?, 'client-a', ?, 'Arrival.2160p.mkv', '', '',
		 1000, ?, '', ?, ?, ?)`, desired1ID, inFlightTransfer, bytesDone, seedTime, seedTime, seedTime)
}

func (h *harness) phaseOf(t *testing.T, id string) string {
	t.Helper()
	var phase string
	if err := h.db.Reader().QueryRow(
		`SELECT phase FROM acquisition_state WHERE desired_item_id = ?`, id).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	return phase
}

func TestSelectOnAQueuedWantReplacesItsGrab(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()
	h.queueOnGood(t, "queued", 0)

	resp := h.doStable(http.MethodPost, "/api/v1/desired/"+desired1ID+"/select",
		strings.NewReader(`{"candidate_id":"plain"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, h.body(resp))
	}
	var got struct {
		CandidateID string `json:"candidate_id"`
		Selected    bool   `json:"selected"`
		Replaced    *struct {
			Provider   string `json:"provider"`
			ExternalID string `json:"external_id"`
		} `json:"replaced"`
	}
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	if got.CandidateID != "plain" || !got.Selected {
		t.Errorf("chosen = %+v", got)
	}
	if got.Replaced == nil || got.Replaced.ExternalID != inFlightTransfer || got.Replaced.Provider != "client-a" {
		t.Fatalf("replaced = %+v, want the queued transfer", got.Replaced)
	}
	if p := h.phaseOf(t, desired1ID); p != "selected" {
		t.Errorf("phase = %s, want selected", p)
	}
	if n := h.countJobs(t, "remove_transfer"); n != 1 {
		t.Errorf("%d remove_transfer jobs, want 1", n)
	}
	if n := h.countJobs(t, "grab_release"); n != 1 {
		t.Errorf("%d grab_release jobs, want 1", n)
	}
	if n := h.countRows(t, `SELECT count(*) FROM acquisitions WHERE desired_item_id = ?`, desired1ID); n != 0 {
		t.Errorf("%d transfer rows left for the want, want 0", n)
	}
}

// A download that has fetched bytes is refused with the state named, and
// replaced with force.
func TestSelectOnADownloadNeedsForce(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()
	h.queueOnGood(t, "downloading", 250)
	path := "/api/v1/desired/" + desired1ID + "/select"

	resp := h.doStable(http.MethodPost, path, strings.NewReader(`{"candidate_id":"plain"}`))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, h.body(resp))
	}
	detail := problemDetail(t, h, resp)
	for _, want := range []string{"DOWNLOADING", "250 of 1000 bytes", "force"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the refusal should mention %q; got: %s", want, detail)
		}
	}
	if p := h.phaseOf(t, desired1ID); p != "downloading" {
		t.Errorf("a refused select moved the want to %s", p)
	}
	if _, c := readCandidates(t, h, desired1ID); c.Selected != "good" {
		t.Errorf("a refused select changed the selection to %q", c.Selected)
	}
	if n := h.countJobs(t, "grab_release"); n != 0 {
		t.Errorf("a refused select queued %d grabs", n)
	}

	forced := h.doStable(http.MethodPost, path, strings.NewReader(`{"candidate_id":"plain","force":true}`))
	if forced.StatusCode != http.StatusOK {
		t.Fatalf("forced status = %d, want 200: %s", forced.StatusCode, h.body(forced))
	}
	if n := h.countJobs(t, "remove_transfer"); n != 1 {
		t.Errorf("%d remove_transfer jobs after a forced select, want 1", n)
	}
}

func TestSelectRefusesARunningSearchAndAnImport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ phase, want string }{
		{"searching", "a search is running"},
		{"verifying", "VERIFYING"},
		{"ingesting", "INGESTING"},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t).seed()
			h.seedCandidates(t)
			h.exec(`UPDATE acquisition_state SET phase = ? WHERE desired_item_id = ?`, tc.phase, desired1ID)
			resp := h.doStable(http.MethodPost, "/api/v1/desired/"+desired1ID+"/select",
				strings.NewReader(`{"candidate_id":"plain","force":true}`))
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", resp.StatusCode, h.body(resp))
			}
			if detail := problemDetail(t, h, resp); !strings.Contains(detail, tc.want) {
				t.Errorf("the refusal should mention %q; got: %s", tc.want, detail)
			}
		})
	}
}

// Blocking the release a want is queued on cancels its grab and re-plans it.
func TestBlockingTheReleaseInFlightCancelsAndReSearches(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()
	h.queueOnGood(t, "queued", 0)

	resp := h.doStable(http.MethodPost, "/api/v1/desired/"+desired1ID+"/blocked",
		strings.NewReader(`{"candidate_id":"good","detail":"wrong language"}`))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, h.body(resp))
	}
	var got struct {
		Cancelled bool `json:"cancelled"`
		Displaced *struct {
			ExternalID string `json:"external_id"`
		} `json:"displaced"`
	}
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Cancelled || got.Displaced == nil || got.Displaced.ExternalID != inFlightTransfer {
		t.Fatalf("block = %+v, want the queued transfer cancelled", got)
	}
	if p := h.phaseOf(t, desired1ID); p != "idle" {
		t.Errorf("phase = %s, want idle", p)
	}
	if n := h.countJobs(t, "remove_transfer"); n != 1 {
		t.Errorf("%d remove_transfer jobs, want 1", n)
	}
	if n := h.countJobs(t, "search_release"); n != 1 {
		t.Errorf("%d search_release jobs, want 1: the want must choose again now", n)
	}

	// Blocking another release leaves an in-flight grab alone.
	h2 := newHarness(t).seed()
	h2.queueOnGood(t, "queued", 0)
	other := h2.doStable(http.MethodPost, "/api/v1/desired/"+desired1ID+"/blocked",
		strings.NewReader(`{"candidate_id":"plain"}`))
	if other.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", other.StatusCode, h2.body(other))
	}
	if strings.Contains(string(h2.body(other)), "cancelled") {
		t.Errorf("blocking a release not in flight reported a cancel: %s", h2.body(other))
	}
	if p := h2.phaseOf(t, desired1ID); p != "queued" {
		t.Errorf("phase = %s, want still queued", p)
	}
}

func TestSelectRefusesRepairHoldWithActionableConflict(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()
	h.seedCandidates(t)
	h.exec(`INSERT INTO acquisition_repair_holds(desired_item_id,detail,created_at) VALUES(?, 'local repair needed', ?)`, desired1ID, seedTime)
	resp := h.doStable(http.MethodPost, "/api/v1/desired/"+desired1ID+"/select", strings.NewReader(`{"candidate_id":"plain"}`))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d: %s", resp.StatusCode, h.body(resp))
	}
	detail := problemDetail(t, h, resp)
	if !strings.Contains(detail, "local storage repair") || !strings.Contains(detail, "reingest") {
		t.Fatalf("not actionable: %s", detail)
	}
}
