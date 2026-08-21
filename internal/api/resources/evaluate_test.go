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

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// Evaluation over HTTP (§63). The domain table proves the scoring; this proves
// the join — a real profile out of a real database, scored against candidates
// that arrived as JSON, with the reasons surviving to the wire.

type wireEvaluation struct {
	QualityProfileID string `json:"quality_profile_id"`
	Selected         string `json:"selected"`
	Ranked           []struct {
		ID       string `json:"id"`
		Accepted bool   `json:"accepted"`
		Score    int    `json:"score"`
		Terminal bool   `json:"terminal"`
		Reasons  []struct {
			Rule    string `json:"rule"`
			Section string `json:"section"`
			Result  string `json:"result"`
			Score   int    `json:"score"`
			Detail  string `json:"detail"`
		} `json:"reasons"`
		RejectedBy []struct {
			Rule   string `json:"rule"`
			Detail string `json:"detail"`
		} `json:"rejected_by"`
	} `json:"ranked"`
}

func evaluate(t *testing.T, h *harness, profileID, body string) (*http.Response, wireEvaluation) {
	t.Helper()
	resp := h.doStable(http.MethodPost,
		"/api/v1/quality-profiles/"+profileID+"/evaluate", strings.NewReader(body))
	var out wireEvaluation
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(h.body(resp), &out); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
	}
	return resp, out
}

// The seeded living-room profile: accept resolution >= 1080; prefer hevc (20)
// and hdr (10); terminal resolution >= 2160 and source == remux.
func TestEvaluateAgainstARealProfile(t *testing.T) {
	h := newHarness(t).seed()

	resp, got := evaluate(t, h, profile1ID, `{"candidates":[
		{"id":"good","title":"Arrival 2160p remux","attributes":{
			"resolution":2160,"source":"remux","video_codec":"hevc","hdr":true}},
		{"id":"ok","title":"Arrival 1080p web","attributes":{
			"resolution":1080,"source":"web-dl","video_codec":"h264","hdr":false}},
		{"id":"bad","title":"Arrival 480p cam","attributes":{
			"resolution":480,"source":"cam","video_codec":"h264","hdr":false}}
	]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	if len(got.Ranked) != 3 {
		t.Fatalf("%d results for 3 candidates", len(got.Ranked))
	}

	if got.Selected != "good" {
		t.Errorf("selected = %q, want good", got.Selected)
	}
	if got.Ranked[0].ID != "good" || !got.Ranked[0].Accepted {
		t.Errorf("best = %+v", got.Ranked[0])
	}
	if got.Ranked[0].Score != 30 {
		t.Errorf("score = %d, want 30 (hevc 20 + hdr 10)", got.Ranked[0].Score)
	}
	if !got.Ranked[0].Terminal {
		t.Error("2160p remux meets both terminal conditions")
	}

	// The middle one is acceptable and not as good as it gets — the gap the
	// upgrade workflow lives in.
	if !got.Ranked[1].Accepted || got.Ranked[1].Terminal {
		t.Errorf("the 1080p web-dl should be accepted and not terminal: %+v", got.Ranked[1])
	}

	// The rejected one is last, and says why.
	last := got.Ranked[2]
	if last.ID != "bad" || last.Accepted {
		t.Fatalf("last = %+v", last)
	}
	if len(last.RejectedBy) == 0 {
		t.Fatal("a rejection with no reason is exactly the opaque scoring §61 rejects")
	}
	var rules []string
	for _, r := range last.RejectedBy {
		rules = append(rules, r.Rule)
		if strings.TrimSpace(r.Detail) == "" {
			t.Errorf("%s rejected with a code and no prose", r.Rule)
		}
	}
	// The seeded living-room fixture has ONE gate — resolution >= 1080 — so
	// that is what rejects. (policy.Defaults() also excludes cams; the
	// fixture is deliberately smaller, and the multi-gate case is covered by
	// the domain table.)
	if len(last.RejectedBy) != 1 || last.RejectedBy[0].Rule != "resolution.gte" {
		t.Errorf("rejected by %v; the fixture's only gate is the resolution", rules)
	}
}

// The claim that makes a gate a gate, over HTTP: maximal preferences cannot buy
// past a failed accept rule.
func TestPreferencesCannotBuyPastAGate(t *testing.T) {
	h := newHarness(t).seed()
	_, got := evaluate(t, h, profile1ID, `{"candidates":[
		{"id":"brilliant-but-tiny","attributes":{
			"resolution":480,"source":"bluray","video_codec":"hevc","hdr":true}}
	]}`)
	if len(got.Ranked) != 1 {
		t.Fatalf("%d results", len(got.Ranked))
	}
	if got.Ranked[0].Accepted {
		t.Fatal("a failed gate rejects whatever the preferences scored")
	}
	if got.Ranked[0].Score != 30 {
		t.Errorf("score = %d — the preferences are still scored, so the reasons are "+
			"complete; the score is simply not used", got.Ranked[0].Score)
	}
	// And nothing was selected.
	if got.Selected != "" {
		t.Errorf("selected = %q; nothing was acceptable", got.Selected)
	}
}

// Twelve candidates, none acceptable, twelve explanations. §63's rejection
// reasons are as much the deliverable as the acceptances.
func TestTwelveRejectionsOverHTTP(t *testing.T) {
	h := newHarness(t).seed()

	var parts []string
	for i := range 12 {
		parts = append(parts, fmt.Sprintf(
			`{"id":"c%02d","attributes":{"resolution":%d,"source":"web-dl","video_codec":"hevc","hdr":true}}`,
			i, 480+i*10))
	}
	_, got := evaluate(t, h, profile1ID, `{"candidates":[`+strings.Join(parts, ",")+`]}`)

	if len(got.Ranked) != 12 {
		t.Fatalf("%d results for 12 candidates", len(got.Ranked))
	}
	if got.Selected != "" {
		t.Errorf("selected = %q when nothing was acceptable", got.Selected)
	}
	for _, r := range got.Ranked {
		if r.Accepted {
			t.Errorf("%s was accepted", r.ID)
		}
		if len(r.RejectedBy) == 0 {
			t.Errorf("%s was rejected with no reason", r.ID)
		}
	}
}

// Determinism over the wire, including under a shuffled input order — the
// property that silently breaks and looks exactly like a working system.
func TestRankingIsStableAcrossInputOrder(t *testing.T) {
	h := newHarness(t).seed()

	tie := func(ids ...string) string {
		var parts []string
		for _, id := range ids {
			parts = append(parts, fmt.Sprintf(
				`{"id":%q,"attributes":{"resolution":2160,"source":"remux","video_codec":"hevc","hdr":true}}`,
				id))
		}
		return `{"candidates":[` + strings.Join(parts, ",") + `]}`
	}

	var want []string
	for _, order := range [][]string{
		{"aaa", "bbb", "ccc"},
		{"ccc", "bbb", "aaa"},
		{"bbb", "aaa", "ccc"},
	} {
		_, got := evaluate(t, h, profile1ID, tie(order...))
		ids := make([]string, len(got.Ranked))
		for i, r := range got.Ranked {
			ids[i] = r.ID
		}
		if want == nil {
			want = ids
			continue
		}
		if strings.Join(ids, ",") != strings.Join(want, ",") {
			t.Fatalf("input order changed the ranking: %v then %v", want, ids)
		}
	}
	if want[0] != "aaa" {
		t.Errorf("ties break to %q; the documented key is the id ascending", want[0])
	}
}

// An attribute the provider could not determine is reported as such, not as a
// failure — and an undetermined GATE still rejects.
func TestUndeterminedAttributesOverTheWire(t *testing.T) {
	h := newHarness(t).seed()

	// No resolution at all.
	_, got := evaluate(t, h, profile1ID, `{"candidates":[
		{"id":"unknown-resolution","attributes":{"source":"remux","video_codec":"hevc","hdr":true}}
	]}`)
	if got.Ranked[0].Accepted {
		t.Error("a gate that cannot be shown to hold must not pass")
	}
	var found bool
	for _, r := range got.Ranked[0].Reasons {
		if r.Rule == "resolution.gte" {
			found = true
			if r.Result != "undetermined" {
				t.Errorf("result = %q, want undetermined — \"it is 720p\" and \"nobody "+
					"could tell\" send an operator to different places", r.Result)
			}
			if !strings.Contains(r.Detail, "could not determine") {
				t.Errorf("detail = %q", r.Detail)
			}
		}
	}
	if !found {
		t.Error("no reason for the undetermined gate")
	}

	// An explicit null means the same as leaving the key out.
	_, alsoNull := evaluate(t, h, profile1ID, `{"candidates":[
		{"id":"unknown-resolution","attributes":{
			"resolution":null,"source":"remux","video_codec":"hevc","hdr":true}}
	]}`)
	if alsoNull.Ranked[0].Accepted {
		t.Error("an explicit null is the same as an absent key")
	}
}

// Every rule considered produces a reason. A rule that ran silently is a rule
// nobody can confirm ran.
func TestEveryRuleProducesAReason(t *testing.T) {
	h := newHarness(t).seed()
	_, got := evaluate(t, h, profile1ID, `{"candidates":[
		{"id":"c1","attributes":{"resolution":2160,"source":"remux","video_codec":"hevc","hdr":true}}
	]}`)
	// The seeded living-room profile has 1 accept + 2 prefer + 2 terminal.
	if len(got.Ranked[0].Reasons) != 5 {
		t.Fatalf("%d reasons for a 5-rule profile", len(got.Ranked[0].Reasons))
	}
	sections := map[string]int{}
	for _, r := range got.Ranked[0].Reasons {
		sections[r.Section]++
		if r.Rule == "" || r.Result == "" || strings.TrimSpace(r.Detail) == "" {
			t.Errorf("incomplete reason: %+v", r)
		}
	}
	if sections["accept"] != 1 || sections["prefer"] != 2 || sections["terminal"] != 2 {
		t.Errorf("sections = %v", sections)
	}
}

// `fail` means exactly one thing. A terminal condition not reached is a MISS,
// and must not appear in rejected_by.
func TestATerminalMissIsNotARejection(t *testing.T) {
	h := newHarness(t).seed()
	_, got := evaluate(t, h, profile1ID, `{"candidates":[
		{"id":"c1","attributes":{"resolution":1080,"source":"web-dl","video_codec":"h264","hdr":false}}
	]}`)
	r := got.Ranked[0]
	if !r.Accepted {
		t.Fatal("1080p web-dl passes the only gate")
	}
	if len(r.RejectedBy) != 0 {
		t.Errorf("an accepted candidate has rejections: %+v", r.RejectedBy)
	}
	var terminalResults []string
	for _, reason := range r.Reasons {
		if reason.Section == "terminal" {
			terminalResults = append(terminalResults, reason.Result)
			if reason.Result == "fail" {
				t.Errorf("%s reported as fail; a terminal condition not reached means "+
					"\"keep looking\", not \"reject\"", reason.Rule)
			}
		}
	}
	if len(terminalResults) != 2 {
		t.Errorf("terminal results = %v", terminalResults)
	}
}

func TestEvaluateRefusals(t *testing.T) {
	cases := []struct {
		name, body, want string
		status           int
	}{
		{"no candidates", `{"candidates":[]}`, "at least one candidate", 400},
		{
			"a candidate with no id",
			`{"candidates":[{"attributes":{"resolution":2160}}]}`,
			"no id", 400,
		},
		{
			"two candidates sharing an id",
			`{"candidates":[{"id":"x","attributes":{}},{"id":"x","attributes":{}}]}`,
			"share the id", 400,
		},
		{
			"an attribute that does not exist",
			`{"candidates":[{"id":"x","attributes":{"bitrate":5000}}]}`,
			"no attribute called", 400,
		},
		{
			"an attribute of the wrong kind",
			`{"candidates":[{"id":"x","attributes":{"resolution":"2160p"}}]}`,
			"is a int attribute", 400,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t).seed()
			resp, _ := evaluate(t, h, profile1ID, tc.body)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.status, h.body(resp))
			}
			if detail := problemDetail(t, h, resp); !strings.Contains(detail, tc.want) {
				t.Errorf("the refusal should mention %q; got: %s", tc.want, detail)
			}
		})
	}
}

func TestEvaluateAgainstAnUnknownProfileIs404(t *testing.T) {
	h := newHarness(t).seed()
	resp, _ := evaluate(t, h, "nope", `{"candidates":[{"id":"x","attributes":{}}]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// Inspectable means inspectable by a read-only credential. An endpoint that
// needed a write token could not be used to tune a profile from a dashboard.
func TestEvaluatingNeedsOnlyRead(t *testing.T) {
	h := newHarness(t, withAuth).seed()
	readToken := h.mint("reader", auth.ScopeRead)

	resp := h.do(http.MethodPost, "/api/v1/quality-profiles/"+profile1ID+"/evaluate",
		readToken.Secret,
		strings.NewReader(`{"candidates":[{"id":"x","attributes":{"resolution":2160}}]}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — evaluation writes nothing: %s",
			resp.StatusCode, h.body(resp))
	}
}

// And it really writes nothing.
func TestEvaluatingWritesNothing(t *testing.T) {
	h := newHarness(t).seed()
	before := h.eventCount(t)
	if resp, _ := evaluate(t, h, profile1ID, `{"candidates":[
		{"id":"x","attributes":{"resolution":2160,"source":"remux"}}]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := h.eventCount(t); got != before {
		t.Errorf("evaluation emitted %d event(s); it changes nothing", got-before)
	}
}
