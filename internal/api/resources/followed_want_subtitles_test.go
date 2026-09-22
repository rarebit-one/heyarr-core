//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// want_subtitles over the real HTTP surface (ADR-0085 §6): set on follow, and
// changed in place by PATCH /followed-sources/{id}.

type followedSubsView struct {
	ID            string   `json:"id"`
	WantSubtitles []string `json:"want_subtitles"`
}

func TestFollowWithWantSubtitles(t *testing.T) {
	h := newHarness(t).seed()

	resp := follow(h, `{"tvdb_id":"12345","title":"The Series","quality_profile":"living-room",`+
		`"backfill":"full","want_subtitles":["EN","en","de"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("follow = %d: %s", resp.StatusCode, h.body(resp))
	}
	var v followedSubsView
	if err := json.Unmarshal(h.body(resp), &v); err != nil {
		t.Fatal(err)
	}
	// Normalised: lower-cased and deduped.
	if len(v.WantSubtitles) != 2 || v.WantSubtitles[0] != "en" || v.WantSubtitles[1] != "de" {
		t.Fatalf("want_subtitles = %v, want [en de]", v.WantSubtitles)
	}

	// PATCH it to a different set.
	patch := h.doStable(http.MethodPatch, "/api/v1/followed-sources/"+v.ID,
		strings.NewReader(`{"want_subtitles":["fr"]}`))
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d: %s", patch.StatusCode, h.body(patch))
	}
	var after followedSubsView
	if err := json.Unmarshal(h.body(patch), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.WantSubtitles) != 1 || after.WantSubtitles[0] != "fr" {
		t.Fatalf("after patch, want_subtitles = %v, want [fr]", after.WantSubtitles)
	}
}
