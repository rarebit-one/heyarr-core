//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The re-probe backfill (#886) queues a probe for each managed video whose
// stored probe cannot say what its picture's dynamic range is, and for no
// other.
func TestProbeBackfillReprobesOnlyWhatCannotSayItsDynamicRange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	const stamp = "2026-11-18T09:00:00Z"
	probeWith := func(blob, streams string) {
		h.exec(`INSERT INTO blob_probes (blob_hash, container, format_long, streams, bytes_read, materialised, probed_at)
			VALUES (?, 'matroska,webm', '', ?, 0, 0, ?)`, blob, streams, stamp)
	}

	blob := func(c string) string { return "blake3:" + strings.Repeat(c, 64) }
	// Probed before colour was recorded: the case the backfill exists for.
	seedBackfillVideo(t, h, "old", "old-e1", blob("a"), false)
	probeWith(blob("a"), `[{"index":0,"type":"video","codec":"hevc","profile":"Main 10","height":2160}]`)
	// Never probed at all.
	seedBackfillVideo(t, h, "none", "none-e1", blob("b"), false)
	// Already says PQ.
	seedBackfillVideo(t, h, "pq", "pq-e1", blob("c"), false)
	probeWith(blob("c"), `[{"index":0,"type":"video","codec":"hevc","height":2160,"color_transfer":"smpte2084"}]`)
	// Dolby Vision profile 5 declares no transfer, and is answered all the same.
	seedBackfillVideo(t, h, "dv5", "dv5-e1", blob("d"), false)
	probeWith(blob("d"), `[{"index":0,"type":"video","codec":"hevc","height":2160,"dolby_vision":{"profile":5,"bl_compatibility_id":0}}]`)
	// Only cover art carries a transfer; the picture itself does not.
	seedBackfillVideo(t, h, "art", "art-e1", blob("e"), false)
	probeWith(blob("e"), `[{"index":0,"type":"video","codec":"mjpeg","attached_picture":true,"color_transfer":"bt709"},`+
		`{"index":1,"type":"video","codec":"h264","height":1080}]`)

	resp := h.do(http.MethodPost, "/api/v1/probes/backfill", "", strings.NewReader(`{"all":true}`))
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
	if out.Candidates != 3 || out.Enqueued != 3 {
		t.Errorf("candidates=%d enqueued=%d, want 3/3 (old, none, art)", out.Candidates, out.Enqueued)
	}
	for _, c := range []string{"a", "b", "e"} {
		if n := h.countRows(t, `SELECT COUNT(*) FROM jobs WHERE type = 'probe_blob' AND dedupe_key = 'probe:'||?`, blob(c)); n != 1 {
			t.Errorf("probe jobs for blob %s = %d, want 1", c, n)
		}
	}
	if n := h.countRows(t, `SELECT COUNT(*) FROM jobs WHERE type = 'probe_blob'`); n != 3 {
		t.Errorf("probe jobs = %d, want 3", n)
	}

	// A second run while those are pending adds nothing: the dedupe key holds.
	resp = h.do(http.MethodPost, "/api/v1/probes/backfill", "", strings.NewReader(`{"work_id":"old"}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("scoped re-run status = %d; body: %s", resp.StatusCode, h.body(resp))
	}
	if n := h.countRows(t, `SELECT COUNT(*) FROM jobs WHERE type = 'probe_blob'`); n != 3 {
		t.Errorf("probe jobs after a re-run = %d, want still 3", n)
	}
}

func TestProbeBackfillRefusesAnUnscopedRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	resp := h.do(http.MethodPost, "/api/v1/probes/backfill", "", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unscoped backfill", resp.StatusCode)
	}
}
