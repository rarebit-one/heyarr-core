// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The quality profile endpoints (§62, M3-01).
//
// The golden files prove the shape. These prove the behaviour that carries the
// issue: that `accept`, `prefer` and `terminal` are three different KINDS of
// statement and the API keeps them from looking like three degrees of one.

func postProfile(t *testing.T, h *harness, body string) *http.Response {
	t.Helper()
	return h.doStable(http.MethodPost, "/api/v1/quality-profiles", strings.NewReader(body))
}

// A gate is not a score. This is the mistake the design most invites — someone
// reading §62's `"hevc": 20` reaches for a weight everywhere — and silently
// ignoring it would leave an operator believing a gate is scoring.
func TestAcceptRuleWithAWeightIsRefused(t *testing.T) {
	h := newHarness(t)
	resp := postProfile(t, h, `{
		"name": "weighted-gate",
		"accept": [{"attribute":"resolution","op":"gte","value":1080,"weight":20}]
	}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	detail := problemDetail(t, h, resp)
	for _, want := range []string{"accept", "GATE", "prefer"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the refusal should mention %q so the operator knows where the rule belongs; got: %s",
				want, detail)
		}
	}
}

func TestTerminalRuleWithAWeightIsRefused(t *testing.T) {
	h := newHarness(t)
	resp := postProfile(t, h, `{
		"name": "weighted-stop",
		"terminal": [{"attribute":"resolution","op":"gte","value":2160,"weight":5}]
	}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if detail := problemDetail(t, h, resp); !strings.Contains(detail, "STOP CONDITION") {
		t.Errorf("the refusal should say a terminal rule is a stop condition; got: %s", detail)
	}
}

// A preference is never a gate. A candidate meeting no preference at all is
// still acceptable — so a profile whose every preference goes unmet is a legal
// profile, and this is the write-time half of that claim: a weight is what
// makes a rule a preference at all.
func TestPreferenceWithoutAWeightIsRefused(t *testing.T) {
	h := newHarness(t)
	resp := postProfile(t, h, `{
		"name": "weightless",
		"prefer": [{"attribute":"hdr","op":"eq","value":true}]
	}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if detail := problemDetail(t, h, resp); !strings.Contains(detail, "non-zero weight") {
		t.Errorf("the refusal should explain why zero is not a weight; got: %s", detail)
	}
}

// A penalty is a real thing to want, so a negative weight is legal. Without
// this, "prefer anything that is not a webrip" has to be written as a gate,
// which rejects rather than deprioritises.
func TestNegativeWeightIsAccepted(t *testing.T) {
	h := newHarness(t)
	resp := postProfile(t, h, `{
		"name": "penalised",
		"prefer": [{"attribute":"source","op":"eq","value":"webrip","weight":-30}]
	}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, h.body(resp))
	}
	var got map[string]any
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	prefer, _ := got["prefer"].([]any)
	if len(prefer) != 1 {
		t.Fatalf("expected the preference to survive, got %v", got["prefer"])
	}
	rule, _ := prefer[0].(map[string]any)
	if w, _ := rule["weight"].(float64); w != -30 {
		t.Errorf("the penalty should round-trip as -30, got %v", rule["weight"])
	}
}

// The acceptance criterion this issue names first: an unknown attribute is
// rejected when the profile is WRITTEN, not when a candidate is evaluated.
func TestUnknownAttributeIsRefusedAtWriteTime(t *testing.T) {
	h := newHarness(t)
	resp := postProfile(t, h, `{
		"name": "typo",
		"accept": [{"attribute":"minimum_resolution","op":"gte","value":1080}]
	}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	detail := problemDetail(t, h, resp)
	if !strings.Contains(detail, "no such attribute") {
		t.Errorf("the refusal should say the attribute does not exist; got: %s", detail)
	}
	// The message lists the real vocabulary. Being told "invalid" and left to
	// guess is the difference between a typo caught in a terminal and a
	// support question.
	if !strings.Contains(detail, "resolution") || !strings.Contains(detail, "video_codec") {
		t.Errorf("the refusal should list the attributes that do exist; got: %s", detail)
	}
}

// A rule that looks right and silently never fires is the worst outcome of the
// three, because nothing complains and the profile appears to work.
func TestRulesThatWouldSilentlyNeverMatchAreRefused(t *testing.T) {
	cases := []struct {
		name string
		rule string
		want string
	}{
		{
			"a list operand with an equality comparison",
			`{"attribute":"source","op":"eq","value":["remux","bluray"]}`,
			`use "in" for a list`,
		},
		{
			"a single name with a set comparison",
			`{"attribute":"source","op":"in","value":"remux"}`,
			"compares against a list",
		},
		{
			"an ordering comparison on a codec name",
			`{"attribute":"video_codec","op":"gte","value":"hevc"}`,
			"does not apply",
		},
		{
			"a quoted number against a numeric attribute",
			`{"attribute":"resolution","op":"gte","value":"1080"}`,
			"takes a int value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			resp := postProfile(t, h, fmt.Sprintf(`{"name":"p","accept":[%s]}`, tc.rule))
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, h.body(resp))
			}
			if detail := problemDetail(t, h, resp); !strings.Contains(detail, tc.want) {
				t.Errorf("the refusal should mention %q; got: %s", tc.want, detail)
			}
		})
	}
}

// An absent `terminal` and an empty one are the same statement, and both are
// legal. "Never stop looking" must not need a sentinel value.
func TestAProfileWithNoTerminalRulesIsLegalAndReadsBackAsAnEmptyList(t *testing.T) {
	h := newHarness(t)
	resp := postProfile(t, h, `{"name":"open-ended","accept":[]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, h.body(resp))
	}
	raw := string(h.body(resp))
	// Never null. A client should not have to handle both null and [] for
	// "this profile has no terminal condition".
	for _, section := range []string{"accept", "prefer", "terminal"} {
		if strings.Contains(raw, `"`+section+`":null`) {
			t.Errorf("%s came back as null rather than []: %s", section, raw)
		}
	}
	if !strings.Contains(raw, `"terminal":[]`) {
		t.Errorf("an absent terminal section should read back as an empty list: %s", raw)
	}
}

// A create, not an upsert. Silently replacing a profile because the name
// matched would discard whatever it said before.
func TestDuplicateNameIsAConflictNotAnOverwrite(t *testing.T) {
	h := newHarness(t)
	if resp := postProfile(t, h, `{"name":"dup","accept":[]}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create: status = %d", resp.StatusCode)
	}
	resp := postProfile(t, h, `{"name":"dup","accept":[
		{"attribute":"resolution","op":"gte","value":2160}]}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, h.body(resp))
	}
	if detail := problemDetail(t, h, resp); !strings.Contains(detail, "dup") {
		t.Errorf("the conflict should name the profile; got: %s", detail)
	}
}

// An omitted section is left alone; a section sent as [] is cleared. Those are
// different intentions, and collapsing them makes "clear the terminal rules"
// and "forget to send them" the same request.
func TestUpdateDistinguishesOmittedFromCleared(t *testing.T) {
	h := newHarness(t).seed()
	path := "/api/v1/quality-profiles/" + profile1ID

	// Omitting `terminal` leaves it alone.
	resp := h.doStable(http.MethodPut, path, strings.NewReader(`{"description":"still a television"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, h.body(resp))
	}
	var after map[string]any
	if err := json.Unmarshal(h.body(resp), &after); err != nil {
		t.Fatal(err)
	}
	if terminal, _ := after["terminal"].([]any); len(terminal) != 2 {
		t.Fatalf("omitting a section must leave it alone, got %v", after["terminal"])
	}

	// Sending it as [] clears it — and the profile becomes one that is never
	// finished, which is a legal and meaningful thing to have just done.
	resp = h.doStable(http.MethodPut, path, strings.NewReader(`{"terminal":[]}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, h.body(resp))
	}
	if err := json.Unmarshal(h.body(resp), &after); err != nil {
		t.Fatal(err)
	}
	if terminal, _ := after["terminal"].([]any); len(terminal) != 0 {
		t.Fatalf("sending [] must clear the section, got %v", after["terminal"])
	}
}

// Invariant 7 — every state transition emits — and its converse, which is
// doing just as much work: something that is not a transition must not.
func TestEventsAreEmittedForChangesAndOnlyForChanges(t *testing.T) {
	h := newHarness(t).seed()
	path := "/api/v1/quality-profiles/" + profile1ID

	before := h.eventCount(t)

	// A real change emits.
	if resp := h.doStable(http.MethodPut, path,
		strings.NewReader(`{"description":"a bigger television"}`)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	afterChange := h.eventCount(t)
	if afterChange != before+1 {
		t.Fatalf("a change should emit exactly one event, went from %d to %d", before, afterChange)
	}

	// Re-sending the identical body does not. A client that re-sends its whole
	// configuration on every start would otherwise turn the stream into a
	// heartbeat, and an event stream that is mostly noise is one nobody
	// follows.
	if resp := h.doStable(http.MethodPut, path,
		strings.NewReader(`{"description":"a bigger television"}`)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := h.eventCount(t); got != afterChange {
		t.Errorf("a no-op update emitted %d event(s); it must emit none", got-afterChange)
	}
}

func TestCreateAndDeleteEmit(t *testing.T) {
	h := newHarness(t)
	before := h.eventCount(t)

	resp := postProfile(t, h, `{"name":"transient","accept":[]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var created map[string]any
	if err := json.Unmarshal(h.body(resp), &created); err != nil {
		t.Fatal(err)
	}
	id, _ := created["id"].(string)

	if got := h.eventCount(t); got != before+1 {
		t.Fatalf("creating a profile should emit one event, got %d", got-before)
	}

	resp = h.doStable(http.MethodDelete, "/api/v1/quality-profiles/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", resp.StatusCode, h.body(resp))
	}
	if got := h.eventCount(t); got != before+2 {
		t.Fatalf("deleting a profile should emit one event, got %d", got-before-1)
	}

	// Deletion is physical here, unlike content (ADR-0018). A profile is a
	// page of configuration; a soft-deleted one would have to be filtered out
	// of every read path forever to stop an operator seeing something they
	// deleted.
	if resp := h.get("/api/v1/quality-profiles/" + id); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a deleted profile should be gone, status = %d", resp.StatusCode)
	}
}

func TestUnknownProfileIs404(t *testing.T) {
	h := newHarness(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		resp := h.doStable(method, "/api/v1/quality-profiles/nope", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s an unknown profile: status = %d, want 404", method, resp.StatusCode)
		}
	}
	resp := h.doStable(http.MethodPut, "/api/v1/quality-profiles/nope", strings.NewReader(`{"name":"x"}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT an unknown profile: status = %d, want 404", resp.StatusCode)
	}
}

// Pagination is keyset like every other collection, so that a profile created
// while someone pages does not shift the boundary.
func TestQualityProfilesPaginate(t *testing.T) {
	h := newHarness(t).seed()
	resp := h.get("/api/v1/quality-profiles?limit=1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var first struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(h.body(resp), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("expected one item and a cursor, got %d items and %q", len(first.Items), first.NextCursor)
	}
	resp = h.get("/api/v1/quality-profiles?limit=1&cursor=" + first.NextCursor)
	var second struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(h.body(resp), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 {
		t.Fatalf("expected a second page, got %d items", len(second.Items))
	}
	if second.Items[0]["name"] == first.Items[0]["name"] {
		t.Error("the second page repeated the first page's row")
	}
}

// problemDetail reads the `detail` out of a problem document, which is where
// the actionable half of a refusal lives.
func problemDetail(t *testing.T, h *harness, resp *http.Response) string {
	t.Helper()
	var p struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(h.body(resp), &p); err != nil {
		t.Fatalf("the refusal is not a problem document: %v", err)
	}
	return p.Detail
}
