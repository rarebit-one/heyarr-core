// Responses in this file are closed by the harness's t.Cleanup, which bodyclose
// cannot see through.
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

type sessionProgress struct {
	Locator string `json:"locator"`
	Unit    string `json:"unit"`
}

type session struct {
	ID        string           `json:"id"`
	AssetID   string           `json:"asset_id"`
	DeviceID  string           `json:"device_id"`
	Verb      string           `json:"verb"`
	State     string           `json:"state"`
	Progress  *sessionProgress `json:"progress"`
	StartedAt *string          `json:"started_at"`
	EndedAt   *string          `json:"ended_at"`
}

func (h *harness) createSession(t *testing.T, body string) (*http.Response, session) {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/v1/consumption/sessions", "", strings.NewReader(body))
	var s session
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(h.body(resp), &s); err != nil {
			t.Fatal(err)
		}
	}
	return resp, s
}

func (h *harness) transition(t *testing.T, id, body string) (*http.Response, session) {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/v1/consumption/sessions/"+id+"/transitions", "",
		strings.NewReader(body))
	var s session
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(h.body(resp), &s); err != nil {
			t.Fatal(err)
		}
	}
	return resp, s
}

// newSession creates one against the seeded asset and device.
func (h *harness) newSession(t *testing.T, verb string) session {
	t.Helper()
	resp, s := h.createSession(t, fmt.Sprintf(
		`{"asset_id":%q,"device_id":%q,"verb":%q}`, asset1ID, device1ID, verb))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("creating a session = %d: %s", resp.StatusCode, h.body(resp))
	}
	return s
}

// A session's whole life, with every transition appearing on the stream in
// order and none missing. Invariant 7 has no exceptions, and the job queue
// needed a follow-up fix (#62) to make that true — this is the same claim,
// asserted before it can be false.
func TestASessionsWholeLifeIsOnTheEventStream(t *testing.T) {
	h := newHarness(t).seed()
	s := h.newSession(t, "watch")

	for _, step := range []struct{ body, wantState string }{
		{`{"transition":"start"}`, "playing"},
		{`{"transition":"progress","progress":{"locator":"12.5","unit":"seconds"}}`, "playing"},
		{`{"transition":"pause"}`, "paused"},
		{`{"transition":"resume"}`, "playing"},
		{`{"transition":"progress","progress":{"locator":"600","unit":"seconds"}}`, "playing"},
		{`{"transition":"complete"}`, "completed"},
	} {
		resp, got := h.transition(t, s.ID, step.body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d: %s", step.body, resp.StatusCode, h.body(resp))
		}
		if got.State != step.wantState {
			t.Fatalf("after %s state = %q, want %q", step.body, got.State, step.wantState)
		}
	}

	evs, err := h.events.Since(t.Context(), 0, []string{"playback.session.*"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range evs {
		types = append(types, e.Type)
	}
	want := []string{
		"playback.session.created",
		"playback.session.started",
		"playback.session.progressed",
		"playback.session.paused",
		"playback.session.resumed",
		"playback.session.progressed",
		"playback.session.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("events =\n  %v\nwant\n  %v", types, want)
	}
}

// An illegal transition is a 409, not a 400 and not a 500, and it emits
// nothing. The distinction matters to a client: a 400 says "you sent nonsense"
// and a 409 says "you are out of date", and only the second is fixed by
// re-reading the session.
func TestAnIllegalTransitionIsAConflictAndEmitsNothing(t *testing.T) {
	h := newHarness(t).seed()
	s := h.newSession(t, "watch")

	if resp, _ := h.transition(t, s.ID, `{"transition":"start"}`); resp.StatusCode != http.StatusOK {
		t.Fatal("could not start")
	}
	if resp, _ := h.transition(t, s.ID, `{"transition":"complete"}`); resp.StatusCode != http.StatusOK {
		t.Fatal("could not complete")
	}

	before := h.eventCount(t)
	resp, _ := h.transition(t, s.ID, `{"transition":"resume"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("resuming a completed session = %d, want 409", resp.StatusCode)
	}
	if body := string(h.body(resp)); !strings.Contains(body, "cannot resume a completed session") {
		t.Errorf("the problem document does not say what is wrong:\n%s", body)
	}
	if after := h.eventCount(t); after != before {
		t.Errorf("a rejected transition emitted %d events", after-before)
	}

	// And the session is untouched.
	var got session
	if err := json.Unmarshal(h.body(h.get("/api/v1/consumption/sessions/"+s.ID)), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "completed" {
		t.Errorf("state = %q after a rejected transition", got.State)
	}
}

func (h *harness) eventCount(t *testing.T) int {
	t.Helper()
	evs, err := h.events.Since(t.Context(), 0, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}

// Resume: the exact locator comes back, for a media timestamp and for a page
// alike. This is the claim ADR-0024 rests on — one model, three units — and it
// is what a "Continue watching" row is made of.
func TestContinueReturnsTheExactLocatorForEveryUnit(t *testing.T) {
	for _, tc := range []struct{ verb, locator, unit string }{
		{"watch", "1284.5", "seconds"},
		{"listen", "37", "seconds"},
		{"read", "epubcfi(/6/14[chap05ref]!/4[body01]/10[para05]/3:10)", "cfi"},
		{"read", "42", "page"},
	} {
		t.Run(tc.verb+"/"+tc.unit, func(t *testing.T) {
			h := newHarness(t).seed()
			s := h.newSession(t, tc.verb)
			h.transition(t, s.ID, `{"transition":"start"}`)
			resp, got := h.transition(t, s.ID, fmt.Sprintf(
				`{"transition":"progress","progress":{"locator":%q,"unit":%q}}`, tc.locator, tc.unit))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("progress = %d: %s", resp.StatusCode, h.body(resp))
			}
			if got.Progress == nil || got.Progress.Locator != tc.locator || got.Progress.Unit != tc.unit {
				t.Fatalf("progress = %+v, want %s/%s", got.Progress, tc.locator, tc.unit)
			}

			// Stopped, not completed: stopping keeps the progress, which is
			// the difference between abandoning and finishing.
			h.transition(t, s.ID, `{"transition":"stop"}`)

			var page struct {
				Items []session `json:"items"`
			}
			if err := json.Unmarshal(h.body(h.get("/api/v1/consumption/sessions?state=stopped")), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 {
				t.Fatalf("stopped sessions = %d", len(page.Items))
			}
			if p := page.Items[0].Progress; p == nil || p.Locator != tc.locator || p.Unit != tc.unit {
				t.Errorf("the resumed locator is %+v, want %s/%s", p, tc.locator, tc.unit)
			}
		})
	}
}

// "Continue watching" as a query: everything not yet terminal, newest first.
func TestResumableExcludesFinishedSessions(t *testing.T) {
	h := newHarness(t).seed()

	open1 := h.newSession(t, "watch")
	h.transition(t, open1.ID, `{"transition":"start"}`)

	done := h.newSession(t, "watch")
	h.transition(t, done.ID, `{"transition":"start"}`)
	h.transition(t, done.ID, `{"transition":"complete"}`)

	abandoned := h.newSession(t, "listen")
	h.transition(t, abandoned.ID, `{"transition":"stop"}`)

	var page struct {
		Items []session `json:"items"`
	}
	if err := json.Unmarshal(h.body(h.get("/api/v1/consumption/sessions?state=resumable")), &page); err != nil {
		t.Fatal(err)
	}

	// Membership, not a count. The seeded catalog already holds a paused
	// session and a completed one, and asserting "exactly one" would make this
	// test a statement about the fixture rather than about the filter — it
	// would break the next time the seed grows, for a reason unrelated to what
	// it is testing.
	got := map[string]string{}
	for _, s := range page.Items {
		got[s.ID] = s.State
	}
	for _, want := range []struct{ id, why string }{
		{open1.ID, "a playing session"},
		{session1ID, "the seeded paused session"},
	} {
		if _, ok := got[want.id]; !ok {
			t.Errorf("resumable omits %s (%v)", want.why, got)
		}
	}
	for _, unwanted := range []struct{ id, why string }{
		{done.ID, "a completed session"},
		{abandoned.ID, "a stopped session"},
		{session2ID, "the seeded completed session"},
	} {
		if state, ok := got[unwanted.id]; ok {
			t.Errorf("resumable includes %s in state %q", unwanted.why, state)
		}
	}
}

// Timestamps distinguish a session nobody watched from one someone watched two
// minutes of. Without that the history is unreadable.
func TestASessionAbandonedBeforeStartingHasNoStartTime(t *testing.T) {
	h := newHarness(t).seed()

	abandoned := h.newSession(t, "watch")
	if abandoned.StartedAt != nil {
		t.Error("a new session already has a start time")
	}
	_, stopped := h.transition(t, abandoned.ID, `{"transition":"stop"}`)
	if stopped.StartedAt != nil {
		t.Error("a session stopped before it started has a start time")
	}
	if stopped.EndedAt == nil {
		t.Error("a stopped session has no end time")
	}

	played := h.newSession(t, "watch")
	_, started := h.transition(t, played.ID, `{"transition":"start"}`)
	if started.StartedAt == nil {
		t.Error("a started session has no start time")
	}
	if started.EndedAt != nil {
		t.Error("a playing session already has an end time")
	}
}

// Nothing recorded is absent, not a zero locator. Position zero is a real
// position, and a client cannot tell "at the very beginning" from "never
// played" if both render as 0.
func TestAFreshSessionHasNoProgressRatherThanZero(t *testing.T) {
	h := newHarness(t).seed()
	s := h.newSession(t, "watch")
	if s.Progress != nil {
		t.Errorf("a new session reports progress %+v", s.Progress)
	}
	raw := string(h.body(h.get("/api/v1/consumption/sessions/" + s.ID)))
	if strings.Contains(raw, `"progress"`) {
		t.Errorf("the progress key is present on a session that has none: %s", raw)
	}
}

func TestSessionRefusals(t *testing.T) {
	h := newHarness(t).seed()
	valid := h.newSession(t, "watch")

	for _, tc := range []struct {
		name, path, body string
		status           int
		want             string
	}{
		{
			"an unknown verb", "/api/v1/consumption/sessions",
			fmt.Sprintf(`{"asset_id":%q,"device_id":%q,"verb":"skim"}`, asset1ID, device1ID),
			400, "verb must be one of",
		},
		{
			"no asset", "/api/v1/consumption/sessions",
			fmt.Sprintf(`{"device_id":%q,"verb":"watch"}`, device1ID),
			400, "asset_id is required",
		},
		{
			// A foreign-key violation is the client naming something that does
			// not exist, which is a 400 with a reason rather than the 500 a
			// raw constraint error would become.
			"an asset that does not exist", "/api/v1/consumption/sessions",
			fmt.Sprintf(`{"asset_id":"nope","device_id":%q,"verb":"watch"}`, device1ID),
			400, "must both name something that exists",
		},
		{
			"a device that does not exist", "/api/v1/consumption/sessions",
			fmt.Sprintf(`{"asset_id":%q,"device_id":"nope","verb":"watch"}`, asset1ID),
			400, "must both name something that exists",
		},
		{
			"an unknown transition", "/api/v1/consumption/sessions/" + valid.ID + "/transitions",
			`{"transition":"rewind"}`, 400, "transition must be one of",
		},
		{
			"no transition", "/api/v1/consumption/sessions/" + valid.ID + "/transitions",
			`{}`, 400, "transition is required",
		},
		{
			"a locator with no unit", "/api/v1/consumption/sessions/" + valid.ID + "/transitions",
			`{"transition":"start","progress":{"locator":"12"}}`, 400, "unit must be one of",
		},
		{
			"a page locator that is not a number", "/api/v1/consumption/sessions/" + valid.ID + "/transitions",
			`{"transition":"start","progress":{"locator":"iv","unit":"page"}}`, 400, "decimal number",
		},
		{
			"a cfi that is not a cfi", "/api/v1/consumption/sessions/" + valid.ID + "/transitions",
			`{"transition":"start","progress":{"locator":"chapter 5","unit":"cfi"}}`, 400, "epubcfi",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(http.MethodPost, tc.path, "", strings.NewReader(tc.body))
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.status, h.body(resp))
			}
			if body := string(h.body(resp)); !strings.Contains(body, tc.want) {
				t.Errorf("the problem document does not say why:\n%s", body)
			}
		})
	}
}

func TestUnknownSessionIsA404(t *testing.T) {
	h := newHarness(t).seed()
	if resp := h.get("/api/v1/consumption/sessions/nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
