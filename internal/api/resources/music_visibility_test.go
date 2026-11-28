package resources_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestRecordingPlaybackRequiresMatchingBlobEvidence(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withRender(testRenderSecret, rendererOrigin, publicOrigin)).seed()
	h.exec(`UPDATE works SET content_type='music', work_key='music-recording:11111111-1111-4111-8111-111111111111', attributes='{}' WHERE id=?`, work1ID)
	check := func(wantVisible bool) {
		t.Helper()
		var d struct {
			PrimaryAsset *struct {
				AssetID string `json:"asset_id"`
			} `json:"primary_asset"`
		}
		if err := json.Unmarshal(h.body(h.get("/api/v1/works/"+work1ID)), &d); err != nil {
			t.Fatal(err)
		}
		if (d.PrimaryAsset != nil) != wantVisible {
			t.Fatalf("primary asset = %+v, want visible %v", d.PrimaryAsset, wantVisible)
		}
		resp, _ := h.startPlayback(t, asset1ID, device1ID, "")
		if (resp.StatusCode == http.StatusCreated) != wantVisible {
			t.Fatalf("playback status = %d, want visible %v", resp.StatusCode, wantVisible)
		}
		resp, _, raw := h.planBody(t, `{"asset_id":"`+asset1ID+`","client":`+phoneSDR+`}`)
		if (resp.StatusCode == http.StatusOK) != wantVisible {
			t.Fatalf("plan status = %d, body %s, want visible %v", resp.StatusCode, raw, wantVisible)
		}
		_, _, caps := h.mintCapabilities(t, `{"asset_ids":["`+asset1ID+`"]}`)
		if len(caps.Capabilities) != 1 || (caps.Capabilities[0].RenderURL != "") != wantVisible {
			t.Fatalf("capabilities = %+v, want visible %v", caps, wantVisible)
		}
	}
	check(false)
	h.exec(`UPDATE works SET attributes=json_object('metadata_matched_blob', ?) WHERE id=?`, blob1Hash, work1ID)
	check(true)
	h.exec(`UPDATE works SET attributes=json_object('metadata_matched_blob', 'blake3:different') WHERE id=?`, work1ID)
	check(false)
}
