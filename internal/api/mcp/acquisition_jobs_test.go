//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package mcp_test

import (
	"strings"
	"testing"
	"time"
)

// get_acquisition_status joins the phase state and the in-flight transfer, so a
// caller sees where a want is AND which release it is pulling — the release name
// carrying the resolution and size that otherwise needed a look in the client.
func TestGetAcquisitionStatus(t *testing.T) {
	h := newHarness(t, false)
	id := h.wantOne("")

	// Put the want in DOWNLOADING behind a 2160p transfer at 50%.
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	h.exec(`UPDATE acquisition_state SET phase = 'downloading' WHERE desired_item_id = ?`, id)
	h.exec(`INSERT INTO acquisitions
		(id, desired_item_id, provider, external_id, external_name,
		 remote_path, local_path, bytes_total, bytes_done, trouble,
		 created_at, updated_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"acq-1", id, "transmission", "infohash:abc",
		"Alien.Earth.2025.S01E01.2160p.WEB-DL.DV.HDR.H265-AOC",
		"", "", int64(10), int64(5), "", stamp, stamp, stamp)

	var out struct {
		Phase    string `json:"phase"`
		State    string `json:"state"`
		Transfer *struct {
			Provider    string  `json:"provider"`
			ReleaseName string  `json:"release_name"`
			BytesTotal  int64   `json:"bytes_total"`
			BytesDone   int64   `json:"bytes_done"`
			PercentDone float64 `json:"percent_done"`
		} `json:"transfer"`
	}
	h.call("", "get_acquisition_status", `{"desired_item_id":"`+id+`"}`).structured(t, &out)

	if out.Phase != "downloading" {
		t.Errorf("phase = %q, want downloading", out.Phase)
	}
	if out.State == "" {
		t.Error("the §64 display name was not rendered")
	}
	if out.Transfer == nil {
		t.Fatal("no transfer reported for a downloading want")
	}
	if out.Transfer.Provider != "transmission" {
		t.Errorf("provider = %q", out.Transfer.Provider)
	}
	if !strings.Contains(out.Transfer.ReleaseName, "2160p") {
		t.Errorf("release_name = %q, want it to carry the 2160p resolution", out.Transfer.ReleaseName)
	}
	if out.Transfer.PercentDone != 50 {
		t.Errorf("percent_done = %v, want 50 (5 of 10 bytes)", out.Transfer.PercentDone)
	}

	// An unknown want is a client error, not an empty status.
	if resp := h.call("", "get_acquisition_status", `{"desired_item_id":"does-not-exist"}`); resp.Body.Error == nil {
		t.Error("an unknown want id should be rejected as a client error")
	}
}

// A want with nothing in flight reports its phase and no transfer.
func TestGetAcquisitionStatusWithoutTransfer(t *testing.T) {
	h := newHarness(t, false)
	id := h.wantOne("")

	var out struct {
		Phase    string `json:"phase"`
		Transfer *struct {
			Provider string `json:"provider"`
		} `json:"transfer"`
	}
	h.call("", "get_acquisition_status", `{"desired_item_id":"`+id+`"}`).structured(t, &out)

	if out.Phase != "idle" {
		t.Errorf("phase = %q, want idle for a fresh want", out.Phase)
	}
	if out.Transfer != nil {
		t.Errorf("transfer = %+v, want none for a want with no download", out.Transfer)
	}
}

// list_jobs filters by state and surfaces the last error — the read behind
// "why is nothing being acquired".
func TestListJobs(t *testing.T) {
	h := newHarness(t, false)
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	h.exec(`INSERT INTO jobs (id, type, state, run_after, created_at, updated_at, last_error)
		VALUES (?, 'search_release', 'dead', ?, ?, ?, 'no indexer could be reached')`,
		"job-dead-1", stamp, stamp, stamp)
	h.exec(`INSERT INTO jobs (id, type, state, run_after, created_at, updated_at)
		VALUES (?, 'grab_release', 'succeeded', ?, ?, ?)`,
		"job-ok-1", stamp, stamp, stamp)

	var out struct {
		Count int `json:"count"`
		Jobs  []struct {
			ID        string  `json:"id"`
			Type      string  `json:"type"`
			State     string  `json:"state"`
			LastError *string `json:"last_error"`
		} `json:"jobs"`
	}
	h.call("", "list_jobs", `{"state":"dead"}`).structured(t, &out)

	var seededDead *string
	for _, j := range out.Jobs {
		if j.State != "dead" {
			t.Errorf("state filter leaked a %q job", j.State)
		}
		if j.ID == "job-ok-1" {
			t.Error("the succeeded job appeared under state=dead")
		}
		if j.ID == "job-dead-1" {
			if j.Type != "search_release" {
				t.Errorf("type = %q, want search_release", j.Type)
			}
			seededDead = j.LastError
		}
	}
	if seededDead == nil || !strings.Contains(*seededDead, "no indexer") {
		t.Errorf("seeded dead job's last_error not surfaced: %v", seededDead)
	}

	// Filtering by type narrows it too.
	h.call("", "list_jobs", `{"type":"grab_release"}`).structured(t, &out)
	for _, j := range out.Jobs {
		if j.Type != "grab_release" {
			t.Errorf("type filter leaked a %q job", j.Type)
		}
	}

	// An invalid state is a client error.
	if resp := h.call("", "list_jobs", `{"state":"bogus"}`); resp.Body.Error == nil {
		t.Error("an invalid state should be rejected")
	}
}
