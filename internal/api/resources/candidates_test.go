// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// Release candidates over HTTP (§60, §63, M3-12).
//
// The persistence layer proves the storage and the worker proves the
// orchestration. What these add is the join: real rows out of a real database,
// through a real socket, with §63's reasons surviving all the way to the wire —
// because a rejection reason that stops at the database answers nobody.

// seedCandidates writes a search's answers directly. The job that produces
// them is tested in internal/worker; these tests need the rows to exist, not
// to exercise how they got there.
func (h *harness) seedCandidates(t *testing.T) {
	t.Helper()
	h.exec(`INSERT INTO release_candidates
		(id, desired_item_id, search_id, provider, candidate_id, title,
		 attributes, evaluation, accepted, score, terminal, selected,
		 overridden, override_detail, searched_at, created_at) VALUES
		(?, ?, 'search-1', 'fake-indexer', 'good', 'Arrival 2160p remux',
		 '{"resolution":2160}',
		 '{"candidate_id":"good","accepted":true,"score":30,"terminal":true,'||
		 '"reasons":[{"rule":"resolution.gte","section":"accept","result":"pass","detail":"resolution 2160, which is at least 1080"}]}',
		 1, 30, 1, 1, 0, '', ?, ?),
		(?, ?, 'search-1', 'fake-indexer', 'plain', 'Arrival 1080p web',
		 '{"resolution":1080}',
		 '{"candidate_id":"plain","accepted":true,"score":0,"terminal":false,'||
		 '"reasons":[{"rule":"resolution.gte","section":"accept","result":"pass","detail":"resolution 1080, which is at least 1080"}]}',
		 1, 0, 0, 0, 0, '', ?, ?),
		(?, ?, 'search-1', 'fake-indexer', 'tiny', 'Arrival 480p cam',
		 '{"resolution":480}',
		 '{"candidate_id":"tiny","accepted":false,"score":0,"terminal":false,'||
		 '"reasons":[{"rule":"resolution.gte","section":"accept","result":"fail","detail":"resolution 480, which is not at least 1080"}]}',
		 0, 0, 0, 0, 0, '', ?, ?)`,
		"01990000-0000-7000-8000-0000000000r1", desired1ID, seedTime, seedTime,
		"01990000-0000-7000-8000-0000000000r2", desired1ID, seedTime, seedTime,
		"01990000-0000-7000-8000-0000000000r3", desired1ID, seedTime, seedTime)
}

type wireCandidates struct {
	DesiredItemID string `json:"desired_item_id"`
	SearchID      string `json:"search_id"`
	Selected      string `json:"selected"`
	Candidates    []struct {
		CandidateID    string `json:"candidate_id"`
		Provider       string `json:"provider"`
		Title          string `json:"title"`
		Accepted       bool   `json:"accepted"`
		Score          int    `json:"score"`
		Terminal       bool   `json:"terminal"`
		Selected       bool   `json:"selected"`
		Overridden     bool   `json:"overridden"`
		OverrideDetail string `json:"override_detail"`
		Reasons        []struct {
			Rule   string `json:"rule"`
			Result string `json:"result"`
			Detail string `json:"detail"`
		} `json:"reasons"`
		RejectedBy []struct {
			Rule   string `json:"rule"`
			Detail string `json:"detail"`
		} `json:"rejected_by"`
	} `json:"candidates"`
}

func readCandidates(t *testing.T, h *harness, id string) (*http.Response, wireCandidates) {
	t.Helper()
	resp := h.get("/api/v1/desired/" + id + "/candidates")
	var out wireCandidates
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(h.body(resp), &out); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
	}
	return resp, out
}

// The rejections reach the wire, with the rule that rejected them. A rejection
// reason that stops at the database answers nobody.
func TestCandidatesCarryTheirReasonsToTheWire(t *testing.T) {
	h := newHarness(t).seed()
	h.seedCandidates(t)

	resp, got := readCandidates(t, h, desired1ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	if len(got.Candidates) != 3 {
		t.Fatalf("%d candidates, want 3", len(got.Candidates))
	}
	if got.Selected != "good" {
		t.Errorf("selected = %q, want good", got.Selected)
	}
	if got.SearchID != "search-1" {
		t.Errorf("search_id = %q", got.SearchID)
	}

	// Best first: accepted above rejected, then score descending.
	var order []string
	for _, c := range got.Candidates {
		order = append(order, c.CandidateID)
	}
	if strings.Join(order, ",") != "good,plain,tiny" {
		t.Errorf("order = %v; accepted before rejected, then score descending", order)
	}

	last := got.Candidates[2]
	if last.Accepted {
		t.Fatal("the 480p candidate should be rejected")
	}
	if len(last.RejectedBy) == 0 {
		t.Fatal("a rejection with no reason is exactly the opaque scoring §61 rejects")
	}
	if last.RejectedBy[0].Rule != "resolution.gte" {
		t.Errorf("rejected by %q, want resolution.gte", last.RejectedBy[0].Rule)
	}
	if strings.TrimSpace(last.RejectedBy[0].Detail) == "" {
		t.Error("a rejection carries a code and no prose")
	}
	// And an accepted candidate carries no rejections.
	if len(got.Candidates[0].RejectedBy) != 0 {
		t.Errorf("an accepted candidate has rejections: %v", got.Candidates[0].RejectedBy)
	}
}

// A want nobody has searched for has no candidates, and that is a 200 with an
// empty list rather than a 404 — the want exists, it simply has no answers yet.
func TestAWantWithNoSearchHasNoCandidates(t *testing.T) {
	h := newHarness(t).seed()
	resp, got := readCandidates(t, h, desired1ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(got.Candidates) != 0 {
		t.Errorf("%d candidates for a want nobody searched", len(got.Candidates))
	}
	if got.Selected != "" {
		t.Errorf("selected = %q", got.Selected)
	}
}

func TestCandidatesForAnUnknownWantIs404(t *testing.T) {
	h := newHarness(t).seed()
	if resp := h.get("/api/v1/desired/nope/candidates"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// A manual search queues a job rather than running one: a search is a job
// (invariant 4), and the worker may be another process.
func TestManualSearchQueuesAJob(t *testing.T) {
	h := newHarness(t).seed()
	resp := h.doStable(http.MethodPost, "/api/v1/desired/"+desired1ID+"/search", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", resp.StatusCode, h.body(resp))
	}
	var got map[string]string
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	if got["job_id"] == "" || got["status"] != "queued" {
		t.Errorf("response = %v", got)
	}
	if n := h.countRows(t,
		`SELECT count(*) FROM jobs WHERE type = 'search_release'`); n != 1 {
		t.Errorf("%d search jobs queued, want 1", n)
	}

	// Deduped per want: asking twice while one is queued yields one job.
	if r := h.doStable(http.MethodPost, "/api/v1/desired/"+desired1ID+"/search", nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", r.StatusCode)
	}
	if n := h.countRows(t,
		`SELECT count(*) FROM jobs WHERE type = 'search_release'`); n != 1 {
		t.Errorf("%d search jobs after asking twice; the dedupe key should collapse them", n)
	}
}

func TestSearchingAnUnknownWantIs404(t *testing.T) {
	h := newHarness(t).seed()
	resp := h.doStable(http.MethodPost, "/api/v1/desired/nope/search", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// The manual override records the disagreement — an override that left no
// trace would look exactly like an ordinary selection.
func TestOverrideRecordsWhatTheScorerHadChosen(t *testing.T) {
	h := newHarness(t).seed()
	h.seedCandidates(t)

	resp := h.doStable(http.MethodPost, "/api/v1/desired/"+desired1ID+"/select",
		strings.NewReader(`{"candidate_id":"plain"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, h.body(resp))
	}
	var chosen struct {
		CandidateID    string `json:"candidate_id"`
		Selected       bool   `json:"selected"`
		Overridden     bool   `json:"overridden"`
		OverrideDetail string `json:"override_detail"`
	}
	if err := json.Unmarshal(h.body(resp), &chosen); err != nil {
		t.Fatal(err)
	}
	if chosen.CandidateID != "plain" || !chosen.Selected || !chosen.Overridden {
		t.Fatalf("chosen = %+v", chosen)
	}
	if !strings.Contains(chosen.OverrideDetail, "good") {
		t.Errorf("the detail should name what the scorer chose; got %q", chosen.OverrideDetail)
	}

	// Exactly one selected afterwards.
	if n := h.countRows(t,
		`SELECT count(*) FROM release_candidates WHERE desired_item_id = ? AND selected = 1`,
		desired1ID); n != 1 {
		t.Errorf("%d candidates selected after an override", n)
	}
	// And it emitted, so an audit of "where did we depart from policy" can
	// subscribe to exactly that.
	if n := h.eventsOfType(t, "acquisition.candidate_overridden"); n != 1 {
		t.Errorf("%d override events, want 1", n)
	}
}

// §62's gates are the operator's own statement of what is acceptable. An
// override that could ignore them would turn `accept` into a suggestion.
func TestOverrideRefusesACandidateTheProfileRejected(t *testing.T) {
	h := newHarness(t).seed()
	h.seedCandidates(t)

	resp := h.doStable(http.MethodPost, "/api/v1/desired/"+desired1ID+"/select",
		strings.NewReader(`{"candidate_id":"tiny"}`))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, h.body(resp))
	}
	detail := problemDetail(t, h, resp)
	if !strings.Contains(detail, "rejected by the quality profile") {
		t.Errorf("the refusal should say why; got: %s", detail)
	}
	if !strings.Contains(detail, "change the profile") {
		t.Errorf("the refusal should say what to do instead; got: %s", detail)
	}

	// The scorer's choice is untouched.
	_, got := readCandidates(t, h, desired1ID)
	if got.Selected != "good" {
		t.Errorf("a refused override changed the selection to %q", got.Selected)
	}
}

func TestOverrideRefusals(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		want       string
	}{
		{"no candidate id", `{}`, 400, "candidate_id is required"},
		{
			"a candidate that was never offered",
			`{"candidate_id":"never-offered"}`, 404, "superseded by a later search",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t).seed()
			h.seedCandidates(t)
			resp := h.doStable(http.MethodPost, "/api/v1/desired/"+desired1ID+"/select",
				strings.NewReader(tc.body))
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.status, h.body(resp))
			}
			if detail := problemDetail(t, h, resp); !strings.Contains(detail, tc.want) {
				t.Errorf("the refusal should mention %q; got: %s", tc.want, detail)
			}
		})
	}
}

// Listing reads what was stored rather than re-scoring. Re-running the scorer
// would answer a different question — what would be decided NOW — which is the
// substitution that makes an audit trail worthless.
func TestListingDoesNotRescore(t *testing.T) {
	h := newHarness(t).seed()
	h.seedCandidates(t)

	// Change the profile out from under the stored evaluations. A listing that
	// re-scored would now reject everything.
	h.exec(`UPDATE quality_profiles SET accept = ? WHERE id = ?`,
		`[{"attribute":"resolution","op":"gte","value":4320}]`, profile1ID)

	_, got := readCandidates(t, h, desired1ID)
	if len(got.Candidates) != 3 {
		t.Fatalf("%d candidates", len(got.Candidates))
	}
	if !got.Candidates[0].Accepted {
		t.Error("the stored evaluation was re-derived against the current profile; " +
			"it must be read back as it was decided")
	}
	if got.Selected != "good" {
		t.Errorf("selected = %q; the recorded decision must survive a profile edit", got.Selected)
	}
}

// Candidates do not outlive the want they explain.
func TestCandidatesCascadeWhenTheWantIsDeleted(t *testing.T) {
	h := newHarness(t).seed()
	h.seedCandidates(t)

	resp := h.doStable(http.MethodDelete, "/api/v1/desired/"+desired1ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if n := h.countRows(t,
		`SELECT count(*) FROM release_candidates WHERE desired_item_id = ?`, desired1ID); n != 0 {
		t.Errorf("%d candidate row(s) outlived their want", n)
	}
}

// Searching and overriding change what will be acquired, so both need `write`.
// Listing explains what already happened, and needs only `read`.
func TestCandidateScopes(t *testing.T) {
	h := newHarness(t, withAuth).seed()
	h.seedCandidates(t)
	reader := h.mint("reader", auth.ScopeRead)

	if resp := h.do(http.MethodGet, "/api/v1/desired/"+desired1ID+"/candidates",
		reader.Secret, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("a read token must be able to read the explanation, status = %d", resp.StatusCode)
	}
	for _, path := range []string{"/search", "/select"} {
		body := strings.NewReader(`{"candidate_id":"plain"}`)
		resp := h.do(http.MethodPost, "/api/v1/desired/"+desired1ID+path, reader.Secret, body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s with a read token: status = %d, want 403", path, resp.StatusCode)
		}
	}
}

// The wire shape is a golden file. A rejection reason's `rule` is the stable
// identity a client branches on, so a change to one has to show up in a
// reviewable diff rather than in a client's error handling six months later.
func TestCandidatesShape(t *testing.T) {
	h := newHarness(t).seed()
	h.seedCandidates(t)
	resp := h.doStable(http.MethodGet, "/api/v1/desired/"+desired1ID+"/candidates", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	testutil.Golden(t, goldenPath("desired_candidates.json"), h.indent(resp))
}
