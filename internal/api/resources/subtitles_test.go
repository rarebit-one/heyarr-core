package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// seedBackfillVideo inserts a work + edition + managed video asset, and returns
// the work id. withSubtitle adds a subtitle asset on the same edition, which is
// what "already captioned" looks like to the backfill filter.
func seedBackfillVideo(t *testing.T, h *harness, workID, videoAsset, videoBlob string, withSubtitle bool) {
	t.Helper()
	const stamp = "2026-09-09T12:00:00Z"
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, created_at, updated_at)
	        VALUES (?, 'series', ?, ?, ?, ?, ?)`, workID, workID, workID, workID, stamp, stamp)
	h.exec(`INSERT INTO editions (id, work_id, created_at) VALUES (?, ?, ?)`, workID+"-ed", workID, stamp)
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1000, 'video/mp4', ?)`, videoBlob, stamp)
	h.exec(`INSERT INTO assets (id, edition_id, source_class, blob_hash, source_path, role, filename, mime, identification_source, created_at, updated_at)
	        VALUES (?, ?, 'managed', ?, ?, 'primary', ?, 'video/mp4', 'scan', ?, ?)`,
		videoAsset, workID+"-ed", videoBlob, "/lib/"+videoAsset+".mp4", videoAsset+".mp4", stamp, stamp)
	if withSubtitle {
		subBlob := "blake3:" + strings.Repeat("f", 64)
		h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 100, 'application/x-subrip', ?)`, subBlob, stamp)
		h.exec(`INSERT INTO assets (id, edition_id, source_class, blob_hash, source_path, role, filename, mime, identification_source, created_at, updated_at)
		        VALUES (?, ?, 'managed', ?, NULL, 'subtitle', ?, 'application/x-subrip', 'scan', ?, ?)`,
			videoAsset+"-sub", workID+"-ed", subBlob, videoAsset+".en.srt", stamp, stamp)
	}
}

func TestSubtitleBackfillEnqueuesForVideosMissingSubtitles(t *testing.T) {
	h := newHarness(t)
	seedBackfillVideo(t, h, "ys", "ys-e1", "blake3:"+strings.Repeat("a", 64), false)

	resp := h.do(http.MethodPost, "/api/v1/subtitles/backfill", "", strings.NewReader(`{"work_id":"ys"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", resp.StatusCode, h.body(resp))
	}
	var out struct {
		Candidates int `json:"candidates"`
		Enqueued   int `json:"enqueued"`
	}
	if err := json.Unmarshal(h.body(resp), &out); err != nil {
		t.Fatal(err)
	}
	if out.Candidates != 1 || out.Enqueued != 1 {
		t.Errorf("candidates=%d enqueued=%d, want 1/1", out.Candidates, out.Enqueued)
	}
	if n := h.countRows(t, `SELECT COUNT(*) FROM jobs WHERE type = 'extract_subtitles'`); n != 1 {
		t.Errorf("extract_subtitles jobs = %d, want 1", n)
	}
}

func TestSubtitleBackfillSkipsAlreadyCaptionedVideos(t *testing.T) {
	h := newHarness(t)
	seedBackfillVideo(t, h, "got", "got-e1", "blake3:"+strings.Repeat("b", 64), true) // has a sidecar

	resp := h.do(http.MethodPost, "/api/v1/subtitles/backfill", "", strings.NewReader(`{"work_id":"got"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", resp.StatusCode, h.body(resp))
	}
	var out struct {
		Candidates int `json:"candidates"`
		Enqueued   int `json:"enqueued"`
	}
	_ = json.Unmarshal(h.body(resp), &out)
	if out.Candidates != 0 || out.Enqueued != 0 {
		t.Errorf("a video that already has a subtitle must not be enqueued, got candidates=%d enqueued=%d", out.Candidates, out.Enqueued)
	}
	if n := h.countRows(t, `SELECT COUNT(*) FROM jobs WHERE type = 'extract_subtitles'`); n != 0 {
		t.Errorf("extract_subtitles jobs = %d, want 0", n)
	}
}

func TestSubtitleBackfillRefusesAnUnscopedRequest(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodPost, "/api/v1/subtitles/backfill", "", strings.NewReader(`{}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unscoped backfill must be 400, got %d", resp.StatusCode)
	}
}
