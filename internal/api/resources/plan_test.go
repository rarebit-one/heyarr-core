//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type planReason struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type plan struct {
	Decision   string        `json:"decision"`
	Reasons    []planReason  `json:"reasons"`
	PeerID     string        `json:"peer_id"`
	Remote     bool          `json:"remote"`
	ContentURL string        `json:"content_url"`
	Routing    *routingBlock `json:"routing"`
}

func (p plan) has(code string) bool {
	for _, r := range p.Reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}

func (h *harness) plan(t *testing.T, assetID, deviceID string) (*http.Response, plan) {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/v1/playback/plan", "",
		strings.NewReader(fmt.Sprintf(`{"asset_id":%q,"device_id":%q}`, assetID, deviceID)))
	var p plan
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(h.body(resp), &p); err != nil {
			t.Fatal(err)
		}
	}
	return resp, p
}

// The seeded catalog has no probe rows, which is not a gap in the fixture —
// it is the state of every blob on a node with no ffprobe (ADR-0023), and the
// planner's answer for it is the one most likely to be got wrong.
func TestPlanningUnprobedMediaIsDirectWithAReason(t *testing.T) {
	h := newHarness(t).seed()

	resp, p := h.plan(t, asset1ID, device1ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	if p.Decision != "direct" {
		t.Errorf("decision = %q, want direct — a node with no probes cannot transcode either, "+
			"so planning transcode would make the whole library unplayable", p.Decision)
	}
	if !p.has("no_probe") {
		t.Errorf("the plan does not say it is a guess: %+v", p.Reasons)
	}
	if p.ContentURL == "" {
		t.Error("a direct plan has no content_url")
	}
	if !strings.Contains(p.ContentURL, "/api/v1/blobs/") {
		t.Errorf("content_url = %q, want the ordinary blob endpoint", p.ContentURL)
	}
}

// With a probe, the planner has something to decide against — every verdict
// reachable through the real API.
func TestPlanningAgainstARealProbe(t *testing.T) {
	for _, tc := range []struct {
		name       string
		container  string
		videoCodec string
		width      int
		device     string
		want       string
		wantReason string
	}{
		{
			name: "matching", container: "mov,mp4,m4a", videoCodec: "h264", width: 1920,
			device: device1ID, want: "direct",
		},
		{
			name: "wrong container, right streams", container: "matroska,webm",
			videoCodec: "h264", width: 1920, device: device1ID, want: "direct",
		},
		{
			name: "a codec the device refuses", container: "mov,mp4,m4a",
			videoCodec: "av1", width: 1920, device: device1ID,
			want: "transcode", wantReason: "video_codec_unsupported",
		},
		{
			name: "a resolution past the device", container: "mov,mp4,m4a",
			videoCodec: "h264", width: 7680, device: device1ID,
			want: "transcode", wantReason: "resolution_too_high",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t).seed()
			h.exec(`INSERT INTO blob_probes
				(blob_hash, container, format_long, duration_seconds, bitrate_bps,
				 streams, bytes_read, materialised, probed_at)
				VALUES (?, ?, '', 120.0, 8000000, ?, 4096, 0, ?)`,
				blob1Hash, tc.container,
				fmt.Sprintf(`[{"index":0,"type":"video","codec":%q,"width":%d,"height":1080},
					{"index":1,"type":"audio","codec":"aac","channels":2}]`, tc.videoCodec, tc.width),
				seedTime)

			_, p := h.plan(t, asset1ID, tc.device)
			if p.Decision != tc.want {
				t.Errorf("decision = %q, want %q (%+v)", p.Decision, tc.want, p.Reasons)
			}
			if tc.wantReason != "" && !p.has(tc.wantReason) {
				t.Errorf("missing reason %q: %+v", tc.wantReason, p.Reasons)
			}
			// A non-direct plan must not hand over the original bytes: doing
			// so invites the client to play exactly what the plan just said it
			// cannot.
			if tc.want != "direct" && p.ContentURL != "" {
				t.Errorf("a %s plan offered content_url %q", tc.want, p.ContentURL)
			}
		})
	}
}

// The mkv case, end to end. A television declaring "mkv" against a file
// ffprobe calls "matroska,webm" must play DIRECT — the planner's first version
// sent every Matroska file in the library to a remux.
func TestMatroskaOnADeviceThatDeclaresMKV(t *testing.T) {
	h := newHarness(t).seed()
	h.exec(`INSERT INTO blob_probes
		(blob_hash, container, format_long, duration_seconds, bitrate_bps,
		 streams, bytes_read, materialised, probed_at)
		VALUES (?, 'matroska,webm', '', 120.0, 8000000, ?, 4096, 0, ?)`,
		blob1Hash,
		`[{"index":0,"type":"video","codec":"h264","width":1920,"height":1080},
		  {"index":1,"type":"audio","codec":"aac","channels":2}]`, seedTime)

	_, p := h.plan(t, asset1ID, device1ID)
	if p.Decision != "direct" {
		t.Errorf("decision = %q, want direct — the seeded device declares mkv (%+v)",
			p.Decision, p.Reasons)
	}
}

func TestPlanRefusals(t *testing.T) {
	h := newHarness(t).seed()
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"no asset", fmt.Sprintf(`{"device_id":%q}`, device1ID), 400},
		{"no device", fmt.Sprintf(`{"asset_id":%q}`, asset1ID), 400},
		{"an unknown asset", fmt.Sprintf(`{"asset_id":"nope","device_id":%q}`, device1ID), 404},
		{"an unknown device", fmt.Sprintf(`{"asset_id":%q,"device_id":"nope"}`, asset1ID), 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(http.MethodPost, "/api/v1/playback/plan", "", strings.NewReader(tc.body))
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d: %s", resp.StatusCode, tc.status, h.body(resp))
			}
		})
	}
}

// Planning writes nothing. A client shows a "play" button by planning and
// opens a session when someone presses it; if planning created state, every
// hover would too.
func TestPlanningOpensNoSession(t *testing.T) {
	h := newHarness(t).seed()

	before := h.count(t, `SELECT count(*) FROM consumption_sessions`)
	h.plan(t, asset1ID, device1ID)
	h.plan(t, asset1ID, device2ID)
	if after := h.count(t, `SELECT count(*) FROM consumption_sessions`); after != before {
		t.Errorf("planning created %d sessions", after-before)
	}
}

func (h *harness) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRowContext(t.Context(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
