package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

// The authoring gap #476 closes, end to end through the CLI against a live API:
// a profile is created from its rule groups and then reads back through the
// same `list` surface. Before this, `quality-profile` was list-only while the
// POST route existed — the same asymmetry `library` had before #475.
func TestQualityProfileCreateAndList(t *testing.T) {
	h := newAPIHarness(t)

	out := h.mustRun("quality-profile", "create", "living-room",
		"--description", "the good TV",
		"--accept", `[{"attribute":"resolution","op":"gte","value":1080}]`,
		"--prefer", `[{"attribute":"video_codec","op":"eq","value":"hevc","weight":20}]`,
		"--json")

	var created client.QualityProfile
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if created.Name != "living-room" || created.ID == "" {
		t.Fatalf("created = %+v", created)
	}
	if created.Seeded {
		t.Errorf("an authored profile must not report as seeded: %+v", created)
	}

	// It reads back through the ordinary list surface, rules and all.
	profiles := listQualityProfiles(t, h)
	got, ok := profileByName(profiles, "living-room")
	if !ok {
		t.Fatalf("created profile not in list: %v", profiles)
	}
	if !strings.Contains(string(got.Accept), "resolution") {
		t.Errorf("accept rule did not round-trip: %s", got.Accept)
	}
	if !strings.Contains(string(got.Prefer), "hevc") {
		t.Errorf("prefer rule did not round-trip: %s", got.Prefer)
	}
}

// A gate is not a score (§62): a weighted `accept` rule is refused server-side,
// and the CLI surfaces that refusal rather than swallowing it.
func TestQualityProfileCreateRejectsWeightedGate(t *testing.T) {
	h := newAPIHarness(t)
	_, stderr, err := h.run("quality-profile", "create", "weighted-gate",
		"--accept", `[{"attribute":"resolution","op":"gte","value":1080,"weight":20}]`)
	if err == nil {
		t.Fatal("a weighted accept rule was accepted")
	}
	if !strings.Contains(stderr, "prefer") && !strings.Contains(err.Error(), "prefer") {
		t.Errorf("refusal should point the operator at prefer: err=%v stderr=%s", err, stderr)
	}
}

// Malformed rule JSON is a local error naming the flag, before any request.
func TestQualityProfileCreateRejectsBadJSON(t *testing.T) {
	h := newAPIHarness(t)
	_, stderr, err := h.run("quality-profile", "create", "broken", "--accept", "{not json")
	if err == nil {
		t.Fatal("malformed --accept JSON was accepted")
	}
	if !strings.Contains(stderr, "accept") && !strings.Contains(err.Error(), "accept") {
		t.Errorf("error should name the offending flag: err=%v stderr=%s", err, stderr)
	}
}

func listQualityProfiles(t *testing.T, h *apiHarness) []client.QualityProfile {
	t.Helper()
	out := h.mustRun("quality-profile", "list", "--json")
	var profiles []client.QualityProfile
	if err := json.Unmarshal([]byte(out), &profiles); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return profiles
}

func profileByName(profiles []client.QualityProfile, name string) (client.QualityProfile, bool) {
	for _, p := range profiles {
		if p.Name == name {
			return p, true
		}
	}
	return client.QualityProfile{}, false
}
