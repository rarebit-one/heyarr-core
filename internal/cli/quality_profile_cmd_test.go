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

// --content-types is comma-separated on the CLI and round-trips as a JSON
// array; set replaces it wholesale like the rule groups, distinguishing
// omitted (leave alone) from an explicit empty value (clear to unrestricted).
func TestQualityProfileContentTypesCreateAndSet(t *testing.T) {
	h := newAPIHarness(t)

	out := h.mustRun("quality-profile", "create", "ebook",
		"--content-types", "book", "--json")
	var created client.QualityProfile
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(created.ContentTypes) != 1 || created.ContentTypes[0] != "book" {
		t.Fatalf("content_types = %v, want [book]", created.ContentTypes)
	}

	h.mustRun("quality-profile", "set", "ebook", "--content-types", "book,music")
	got, ok := profileByName(listQualityProfiles(t, h), "ebook")
	if !ok {
		t.Fatal("profile vanished after set")
	}
	if len(got.ContentTypes) != 2 || got.ContentTypes[0] != "book" || got.ContentTypes[1] != "music" {
		t.Fatalf("content_types after set = %v, want [book music]", got.ContentTypes)
	}

	// An empty --content-types clears it back to unrestricted, distinct from
	// never passing the flag at all.
	h.mustRun("quality-profile", "set", "ebook", "--content-types", "")
	got, _ = profileByName(listQualityProfiles(t, h), "ebook")
	if len(got.ContentTypes) != 0 {
		t.Fatalf("an empty --content-types must clear to unrestricted, got %v", got.ContentTypes)
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

// set changes only the rule groups it is given: a group left off is preserved.
// This is what makes "add a language preference to everyday" a one-liner rather
// than a create-a-near-clone-and-repoint-every-source (#559).
func TestQualityProfileSetChangesOnlyGivenGroups(t *testing.T) {
	h := newAPIHarness(t)
	h.mustRun("quality-profile", "create", "everyday-x",
		"--accept", `[{"attribute":"resolution","op":"gte","value":720}]`,
		"--prefer", `[{"attribute":"video_codec","op":"eq","value":"hevc","weight":15}]`,
		"--terminal", `[{"attribute":"resolution","op":"gte","value":1080}]`)

	h.mustRun("quality-profile", "set", "everyday-x",
		"--prefer", `[{"attribute":"language","op":"eq","value":"en","weight":50}]`)

	got, ok := profileByName(listQualityProfiles(t, h), "everyday-x")
	if !ok {
		t.Fatal("profile vanished after set")
	}
	if !strings.Contains(string(got.Prefer), "language") {
		t.Errorf("prefer was not replaced: %s", got.Prefer)
	}
	if !strings.Contains(string(got.Accept), "resolution") {
		t.Errorf("accept should be left alone by a prefer-only set: %s", got.Accept)
	}
	if !strings.Contains(string(got.Terminal), "resolution") {
		t.Errorf("terminal should be left alone by a prefer-only set: %s", got.Terminal)
	}
}

// An explicit empty array clears a group — a different intention from omitting
// the flag, which is the distinction the pointer request body preserves.
func TestQualityProfileSetEmptyArrayClears(t *testing.T) {
	h := newAPIHarness(t)
	h.mustRun("quality-profile", "create", "clearme",
		"--accept", `[{"attribute":"resolution","op":"gte","value":720}]`,
		"--terminal", `[{"attribute":"resolution","op":"gte","value":1080}]`)

	h.mustRun("quality-profile", "set", "clearme", "--terminal", `[]`)

	got, _ := profileByName(listQualityProfiles(t, h), "clearme")
	if body := strings.TrimSpace(string(got.Terminal)); body != "" && body != "[]" && body != "null" {
		t.Errorf("terminal should have been cleared, got %s", got.Terminal)
	}
	if !strings.Contains(string(got.Accept), "resolution") {
		t.Errorf("accept should survive a terminal-only clear: %s", got.Accept)
	}
}

// set resolves an id as readily as a name, and --name renames.
func TestQualityProfileSetByIDAndRename(t *testing.T) {
	h := newAPIHarness(t)
	out := h.mustRun("quality-profile", "create", "oldname",
		"--accept", `[{"attribute":"resolution","op":"gte","value":720}]`, "--json")
	var created client.QualityProfile
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	h.mustRun("quality-profile", "set", created.ID, "--name", "newname")

	profiles := listQualityProfiles(t, h)
	if _, ok := profileByName(profiles, "newname"); !ok {
		t.Error("rename by id did not take")
	}
	if _, ok := profileByName(profiles, "oldname"); ok {
		t.Error("old name still present after rename")
	}
}

// Malformed rule JSON is a local error naming the flag, before any request.
func TestQualityProfileSetRejectsBadJSON(t *testing.T) {
	h := newAPIHarness(t)
	h.mustRun("quality-profile", "create", "victim",
		"--accept", `[{"attribute":"resolution","op":"gte","value":720}]`)
	_, stderr, err := h.run("quality-profile", "set", "victim", "--prefer", "{not json")
	if err == nil {
		t.Fatal("malformed --prefer JSON was accepted")
	}
	if !strings.Contains(stderr, "prefer") && !strings.Contains(err.Error(), "prefer") {
		t.Errorf("refusal should name the flag: err=%v stderr=%s", err, stderr)
	}
}

// Setting a profile that does not exist names the identifier the operator gave.
func TestQualityProfileSetUnknownProfile(t *testing.T) {
	h := newAPIHarness(t)
	_, stderr, err := h.run("quality-profile", "set", "ghost", "--description", "x")
	if err == nil {
		t.Fatal("set on a missing profile succeeded")
	}
	if !strings.Contains(stderr, "ghost") && !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the missing profile: err=%v stderr=%s", err, stderr)
	}
}
