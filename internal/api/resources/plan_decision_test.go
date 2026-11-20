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
