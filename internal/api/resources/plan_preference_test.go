//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A person's preferred quality on the plan request (#887). The node cannot
// read the synced preference (personal state, Invariant 6), so the client
// resolves it with this device's override and sends the effective one.

type preferredPlan struct {
	selectedPlan
	Preference *struct {
		Quality       string `json:"quality"`
		MaxHeight     int64  `json:"max_height"`
		MaxBitrateBPS int64  `json:"max_bitrate_bps"`
	} `json:"preference"`
}

func (h *harness) planPreferred(t *testing.T, body string) preferredPlan {
	t.Helper()
	resp, _, raw := h.planBody(t, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	var p preferredPlan
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("undecodable plan: %v\n%s", err, raw)
	}
	return p
}

// An HDR phone with a 1080-line screen: fit takes the 1080p copy, as #885
// does; best takes the 2160p copy and plays it as it is, with no downscale
// back to the screen; and the answer says which preference it applied.
func TestPlanByWorkUnderBestQuality(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()
	const hdrPhone = `{"containers":["mp4","mkv"],"video":["h264","hevc"],"audio":["aac","eac3"],"max_height":1080,"supports_hdr":true}`

	fit := h.planPreferred(t, `{"work_id":"`+work1ID+`","client":`+hdrPhone+`,"preference":{"quality":"fit"}}`)
	if fit.AssetID != assetFHDID {
		t.Fatalf("fit: asset_id = %s, want the 1080p copy", fit.AssetID)
	}
	if fit.Preference == nil || fit.Preference.Quality != "fit" {
		t.Errorf("fit: preference echo = %+v, want fit", fit.Preference)
	}

	best := h.planPreferred(t, `{"work_id":"`+work1ID+`","client":`+hdrPhone+`,"preference":{"quality":"best"}}`)
	if best.AssetID != asset1ID {
		t.Fatalf("best: asset_id = %s, want the 2160p copy", best.AssetID)
	}
	if best.Selection == nil || !strings.Contains(best.Selection.Chosen.Reason.Detail, "best quality") {
		t.Errorf("best: the chosen reason does not name the preference: %+v", best.Selection)
	}
	if best.Mode != "direct" {
		t.Errorf("best: mode = %q (%q), want direct: best quality is not scaled down to the screen", best.Mode, best.Reason)
	}
	if best.Preference == nil || best.Preference.Quality != "best" {
		t.Errorf("best: preference echo = %+v", best.Preference)
	}
}

// Best quality bounds the asset_id plan too: the copy a person asked for by
// name is not downscaled to the screen, and a cap of their own still holds.
func TestPlanByAssetUnderBestQuality(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()

	fit := h.planPreferred(t, `{"asset_id":"`+asset1ID+`","client":`+phoneSDR+`}`)
	best := h.planPreferred(t, `{"asset_id":"`+asset1ID+`","client":`+phoneSDR+`,"preference":{"quality":"best"}}`)
	capped := h.planPreferred(t, `{"asset_id":"`+asset1ID+`","client":`+phoneSDR+`,"preference":{"quality":"best","max_height":1080}}`)

	if fit.Preference != nil {
		t.Errorf("a request with no preference was echoed one: %+v", fit.Preference)
	}
	if fit.Mode == "direct" && fit.Reason == "" {
		t.Fatalf("fit: the 2160p copy on a 1080 screen played clean direct; the test no longer contrasts")
	}
	if best.Mode != "direct" || strings.Contains(best.Reason, "2160") {
		t.Errorf("best: mode = %q (%q), want direct with no resolution complaint", best.Mode, best.Reason)
	}
	if capped.Reason != fit.Reason || capped.Mode != fit.Mode {
		t.Errorf("best under a 1080 cap = %q (%q), want the fit answer %q (%q)", capped.Mode, capped.Reason, fit.Mode, fit.Reason)
	}
	if capped.Preference == nil || capped.Preference.MaxHeight != 1080 {
		t.Errorf("capped: preference echo = %+v", capped.Preference)
	}
}

// A data saver on a 1080-line phone: nothing is at or under 720, so the
// smaller copy is chosen and planned for 720 lines, and the reason says why.
func TestPlanByWorkUnderDataSaver(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()

	p := h.planPreferred(t, `{"work_id":"`+work1ID+`","client":`+phoneSDR+`,"preference":{"quality":"data_saver"}}`)
	if p.AssetID != assetFHDID || p.Selection == nil {
		t.Fatalf("asset_id = %s, want the 1080p copy", p.AssetID)
	}
	chosen := p.Selection.Chosen.Reason
	if chosen.Code != "smallest_over_limit" || !strings.Contains(chosen.Detail, "max height 720 (data saver)") {
		t.Errorf("chosen reason = %+v", chosen)
	}
	if p.Mode == "direct" && p.Reason == "" {
		t.Errorf("a 1080p copy under a 720 data saver played clean direct: the leg ignored the preference")
	}
}

// A device that registered once and declares nothing per request still has
// its stored height laid under the person's preference.
func TestPlanByWorkForADeviceUnderAPreference(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()
	// device1 is a 2160p HDR television.
	tv := h.planPreferred(t, `{"work_id":"`+work1ID+`","device_id":"`+device1ID+`"}`)
	saver := h.planPreferred(t, `{"work_id":"`+work1ID+`","device_id":"`+device1ID+`","preference":{"quality":"data_saver"}}`)
	if tv.AssetID != asset1ID {
		t.Fatalf("television: asset_id = %s, want the 2160p copy", tv.AssetID)
	}
	if saver.AssetID != assetFHDID {
		t.Errorf("television, data saver: asset_id = %s, want the 1080p copy", saver.AssetID)
	}
	if saver.Preference == nil || saver.Preference.Quality != "data_saver" {
		t.Errorf("preference echo = %+v", saver.Preference)
	}
}

func TestPlanPreferenceRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()
	cases := []struct{ name, pref, detail string }{
		{"an unknown quality", `{"quality":"fit_screen"}`, "preference.quality must be fit, best or data_saver"},
		{"a negative height", `{"max_height":-1}`, "must not be negative"},
		{"a negative bitrate", `{"quality":"best","max_bitrate_bps":-5}`, "must not be negative"},
	}
	for _, tc := range cases {
		resp, _, raw := h.planBody(t, `{"work_id":"`+work1ID+`","client":`+phoneSDR+`,"preference":`+tc.pref+`}`)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), tc.detail) {
			t.Errorf("%s: status %d, body %s; want 400 naming %q", tc.name, resp.StatusCode, raw, tc.detail)
		}
	}
}

// A registered device planned by asset under a preference: the planner holds
// the picture to the width and the height together, so both move (Codex P2
// on #893). A data saver on the 2160p television refuses the 1080p copy as too
// tall, and so does one on a device that never stated a frame at all.
func TestPlanByAssetForADeviceUnderAPreference(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedTwoCopies()
	hasResolution := func(p preferredPlan) bool {
		for _, r := range p.Reasons {
			if r.Code == "resolution_too_high" {
				return true
			}
		}
		return false
	}
	body := `{"asset_id":"` + assetFHDID + `","device_id":"` + device1ID + `"`
	if hasResolution(h.planPreferred(t, body+`}`)) {
		t.Fatalf("the 1080p copy is too tall for the 2160p television without a preference")
	}
	if !hasResolution(h.planPreferred(t, body+`,"preference":{"quality":"data_saver"}}`)) {
		t.Errorf("data saver: the 1080p copy passed a 720-line limit")
	}
	h.exec(`UPDATE devices SET max_width = 0, max_height = 0 WHERE id = ?`, device1ID)
	if !hasResolution(h.planPreferred(t, body+`,"preference":{"quality":"data_saver"}}`)) {
		t.Errorf("data saver on a device with no stated frame: the 1080p copy passed a 720-line limit")
	}
}
