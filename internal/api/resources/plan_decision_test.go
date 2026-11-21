//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"net/http"
	"testing"
)

// The plan's decision accounts for the client's height cap (#892). A copy
// chosen as smallest_over_limit is scaled down by the leg, and a decision of
// direct with no reasons told a client that reads `decision` that nothing
// would be re-encoded.
func TestThePlanDecisionAccountsForTheHeightCap(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		body      string
		wantAsset string
		want      string
	}{
		{
			// The issue: the only copy in reach is taller than the cap.
			name:      "chosen over the limit",
			body:      `{"work_id":"` + work1ID + `","edition_id":"` + edition1ID + `","client":` + phoneSDR + `}`,
			wantAsset: asset1ID, want: "transcode",
		},
		{
			name:      "chosen within the limit",
			body:      `{"work_id":"` + work1ID + `","client":` + phoneSDR + `}`,
			wantAsset: assetFHDID, want: "direct",
		},
		{
			// A registered 2160-line television, asked for at most 1080: the
			// leg scales to the request's cap, so the decision is held to it.
			name: "registered device, client cap under it",
			body: `{"asset_id":"` + asset1ID + `","device_id":"` + device1ID +
				`","client":{"containers":["mkv"],"video":["hevc"],"audio":["eac3"],"max_height":1080}}`,
			wantAsset: asset1ID, want: "transcode",
		},
		{
			name: "registered device, client cap at it",
			body: `{"asset_id":"` + asset1ID + `","device_id":"` + device1ID +
				`","client":{"containers":["mkv"],"video":["hevc"],"audio":["eac3"],"max_height":2160}}`,
			wantAsset: asset1ID, want: "direct",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t).seed().seedTwoCopies()

			resp, p, raw := h.planBody(t, tc.body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.StatusCode, raw)
			}
			if p.AssetID != tc.wantAsset {
				t.Fatalf("asset_id = %s, want %s\n%s", p.AssetID, tc.wantAsset, raw)
			}
			if p.Decision != tc.want {
				t.Fatalf("decision = %q (reasons %+v), want %q\n%s", p.Decision, p.Reasons, tc.want, raw)
			}
			if got := p.has("resolution_too_high"); got != (tc.want == "transcode") {
				t.Errorf("resolution_too_high in reasons = %v, want %v: %+v", got, tc.want == "transcode", p.Reasons)
			}
		})
	}
}

// The plan and the leg read the same height, whichever side states it (#931).
// With a device_id and a client that leaves max_height unset, the planner held
// the picture to the registered device's height while the leg had no cap, so
// a 2160-line copy for a 1080-line television said transcode and was handed
// over direct. ADR-0123's field-by-field rule fills the leg's height from the
// device as well. Across cap × device, `decision: transcode` (for the height)
// is exactly `mode: stream`.
func TestThePlanAndTheLegAgreeOnTheHeight(t *testing.T) {
	t.Parallel()
	const (
		hdTVID = "01990000-0000-7000-8000-0000000000d3"
		caps   = `"containers":["mkv"],"video":["hevc","h264"],"audio":["eac3","aac"],"supports_hdr":true`
	)
	for _, tc := range []struct {
		name   string
		device string
		cap    string
		asset  string
		stream bool
	}{
		{"no device, no cap", "", "", asset1ID, false},
		{"no device, cap under the copy", "", `,"max_height":1080`, asset1ID, true},
		{"no device, cap over the copy", "", `,"max_height":2160`, assetFHDID, false},
		// The issue: the registered device's height binds the leg too.
		{"1080-line device, no cap, a 2160-line copy", hdTVID, "", asset1ID, true},
		{"1080-line device, no cap, a 1080-line copy", hdTVID, "", assetFHDID, false},
		{"1080-line device, cap under it", hdTVID, `,"max_height":720`, assetFHDID, true},
		{"1080-line device, cap over it", hdTVID, `,"max_height":2160`, asset1ID, true},
		{"2160-line device, no cap", device1ID, "", asset1ID, false},
		{"2160-line device, cap under it", device1ID, `,"max_height":1080`, asset1ID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, withStreamLeg(&fakeStreamer{}, newFakeBlobs(t), nil)).seed().seedTwoCopies()
			h.exec(`INSERT INTO devices
				(id, device_key, name, platform, max_width, max_height, max_bitrate_bps, supports_hdr,
				 containers, video_codecs, audio_codecs, created_at, updated_at, last_seen_at)
				VALUES (?, 'tv-bedroom', 'Bedroom', 'tvos', 1920, 1080, 120000000, 1,
				 '["mkv"]', '["h264","hevc"]', '["aac","eac3"]', ?, ?, ?)`, hdTVID, seedTime, seedTime, seedTime)

			body := `{"asset_id":"` + tc.asset + `"`
			if tc.device != "" {
				body += `,"device_id":"` + tc.device + `"`
			}
			body += `,"client":{` + caps + tc.cap + `}}`
			resp, p, raw := h.planBody(t, body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.StatusCode, raw)
			}
			wantMode, wantDecision := "direct", "direct"
			if tc.stream {
				wantMode, wantDecision = "stream", "transcode"
			}
			if p.Mode != wantMode || p.Decision != wantDecision {
				t.Fatalf("mode %q, decision %q (reasons %+v), want %q and %q\n%s",
					p.Mode, p.Decision, p.Reasons, wantMode, wantDecision, raw)
			}
			if got := p.has("resolution_too_high"); got != tc.stream {
				t.Errorf("resolution_too_high in reasons = %v, want it exactly when the leg scales: %+v", got, p.Reasons)
			}
		})
	}
}
