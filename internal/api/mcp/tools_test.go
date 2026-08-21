//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package mcp_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every shipped tool, exercised end to end over a real router against a real
// database.

func TestInitialize(t *testing.T) {
	h := newHarness(t, false)
	resp := h.rpc("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)

	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Instructions string                     `json:"instructions"`
	}
	if err := json.Unmarshal(resp.Body.Result, &result); err != nil {
		t.Fatalf("initialize did not answer: %s", resp.Raw)
	}
	if result.ProtocolVersion == "" || result.ServerInfo.Name != "heyarr" {
		t.Errorf("handshake = %+v", result)
	}
	// Tools and nothing else. A `resources` capability would be the second
	// read API ADR-0019 exists to prevent.
	if _, ok := result.Capabilities["tools"]; !ok {
		t.Error("no tools capability was advertised")
	}
	for _, unwanted := range []string{"resources", "prompts"} {
		if _, ok := result.Capabilities[unwanted]; ok {
			t.Errorf("%q is advertised; this server is semantic actions, not a browsable API",
				unwanted)
		}
	}
	// The instructions carry §72's boundary, because an agent that does not
	// know what this server cannot see will guess.
	if !strings.Contains(strings.ToLower(result.Instructions), "personal state") {
		t.Error("the instructions should tell an agent what this server cannot see")
	}
}

func TestToolsListIsStableAndDescribesItself(t *testing.T) {
	h := newHarness(t, false)

	var first []string
	for range 5 {
		resp := h.rpc("", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		var result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
				Annotations struct {
					ReadOnlyHint  bool   `json:"readOnlyHint"`
					RequiredScope string `json:"heyarr/requiredScope"`
				} `json:"annotations"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(resp.Body.Result, &result); err != nil {
			t.Fatalf("tools/list did not answer: %s", resp.Raw)
		}
		if len(result.Tools) == 0 {
			t.Fatal("no tools were listed")
		}

		names := make([]string, 0, len(result.Tools))
		for _, tool := range result.Tools {
			names = append(names, tool.Name)
			if tool.Description == "" || tool.InputSchema == nil {
				t.Errorf("%s is listed without a description or a schema", tool.Name)
			}
			// The required scope is on the descriptor so an agent can tell
			// "no such verb" from "not with this token" before calling.
			if tool.Annotations.RequiredScope == "" {
				t.Errorf("%s does not say what scope it needs", tool.Name)
			}
		}
		// Stable ordering: an agent diffing the surface between calls should
		// see a change only when the surface changed, and Go randomises map
		// iteration.
		if first == nil {
			first = names
			continue
		}
		if strings.Join(names, ",") != strings.Join(first, ",") {
			t.Fatalf("tools/list is not stable: %v then %v", first, names)
		}
	}
}

// The whole surface is listed whatever the caller holds. A verb that vanished
// for a read token would make the vocabulary depend on the credential, so an
// agent could not learn what exists.
func TestToolsListDoesNotDependOnTheCredential(t *testing.T) {
	h := newHarness(t, true)
	read := h.mint("reader", "read")
	admin := h.mint("admin", "read", "write", "admin")

	list := func(token string) string {
		resp := h.rpc(token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		var result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(resp.Body.Result, &result); err != nil {
			t.Fatalf("tools/list: %s", resp.Raw)
		}
		names := make([]string, 0, len(result.Tools))
		for _, tool := range result.Tools {
			names = append(names, tool.Name)
		}
		return strings.Join(names, ",")
	}
	if list(read) != list(admin) {
		t.Error("the tool surface differs by credential, so an agent cannot learn " +
			"what exists")
	}
}

func TestSearchContent(t *testing.T) {
	h := newHarness(t, false)

	var out struct {
		Count int `json:"count"`
		Works []struct {
			WorkID string `json:"work_id"`
			Title  string `json:"title"`
		} `json:"works"`
	}
	h.call("", "search_content", `{"query":"arrival"}`).structured(t, &out)
	if out.Count != 1 || out.Works[0].WorkID != workID {
		t.Fatalf("search found %+v", out)
	}

	// Matched against the normalised form, so case and articles do not matter.
	h.call("", "search_content", `{"query":"ARRIVAL"}`).structured(t, &out)
	if out.Count != 1 {
		t.Errorf("search is case-sensitive, so an agent must guess the casing")
	}

	// A search with nothing to search on is refused rather than returning the
	// whole library.
	resp := h.call("", "search_content", `{}`)
	if resp.Body.Error == nil {
		t.Error("a search with no query and no content type should be refused")
	}
}

// The central action, and the one that must work for content nothing has seen.
func TestWantContentForSomethingTheLibraryHasNeverSeen(t *testing.T) {
	h := newHarness(t, false)

	var item struct {
		ID          string `json:"id"`
		WorkID      string `json:"work_id"`
		Monitor     bool   `json:"monitor"`
		Acquisition *struct {
			State string `json:"state"`
		} `json:"acquisition"`
	}
	h.call("", "want_content", `{
		"title":"The Conversation","content_type":"movie","year":1974,
		"quality_profile":"living-room"}`).structured(t, &item)

	if item.ID == "" || item.WorkID == "" {
		t.Fatalf("wanting unknown content produced %+v", item)
	}
	if !item.Monitor {
		t.Error("monitoring should default to true — wanting something and never " +
			"looking again is the surprising default")
	}
	// The acquisition state is created in the same transaction, through the
	// SAME intent the HTTP door uses. A want with no acquisition row is one
	// the reconciliation sweep cannot advance.
	if item.Acquisition == nil || item.Acquisition.State != "MISSING" {
		t.Fatalf("no acquisition state was created: %+v", item.Acquisition)
	}
}

// The profile is named the way a person names it, not by id.
func TestWantContentRefusesAnUnknownProfileByName(t *testing.T) {
	h := newHarness(t, false)
	resp := h.call("", "want_content",
		`{"work_id":"`+workID+`","quality_profile":"nonexistent"}`)
	if resp.Body.Error == nil {
		t.Fatal("an unknown profile should be refused")
	}
	if resp.Body.Error.Code != -32602 {
		t.Errorf("code = %d, want -32602 (invalid params)", resp.Body.Error.Code)
	}
	if !strings.Contains(resp.Body.Error.Message, "nonexistent") {
		t.Errorf("the refusal should name what was not found; got %q",
			resp.Body.Error.Message)
	}
}

// §61: two wants over one target with different profiles are the point; the
// same profile twice is one want written twice. The conflict reaches the agent
// as its fault, not as an internal error — which is what resources.ClientFault
// exists to keep consistent across both doors.
func TestWantingTheSameThingTwiceIsTheCallersFault(t *testing.T) {
	h := newHarness(t, false)
	h.wantOne("")

	resp := h.call("", "want_content",
		`{"work_id":"`+workID+`","quality_profile":"living-room"}`)
	if resp.Body.Error == nil {
		t.Fatal("a duplicate want should be refused")
	}
	if resp.Body.Error.Code != -32602 {
		t.Errorf("code = %d, want -32602 — a duplicate is the caller's mistake, "+
			"not ours", resp.Body.Error.Code)
	}
	if !strings.Contains(resp.Body.Error.Message, "different profile") {
		t.Errorf("the refusal should say how to want a second copy; got %q",
			resp.Body.Error.Message)
	}
}

func TestMonitorContent(t *testing.T) {
	h := newHarness(t, false)
	id := h.wantOne("")

	var item struct {
		Monitor bool `json:"monitor"`
	}
	h.call("", "monitor_content",
		`{"desired_item_id":"`+id+`","monitor":false}`).structured(t, &item)
	if item.Monitor {
		t.Error("monitoring was not turned off")
	}

	// Required rather than defaulted: "monitor_content" with no value is
	// ambiguous between on and off, and guessing is a change nobody asked for.
	resp := h.call("", "monitor_content", `{"desired_item_id":"`+id+`"}`)
	if resp.Body.Error == nil {
		t.Error("monitor_content with no value should be refused rather than guessed")
	}
}

func TestGetMissingContent(t *testing.T) {
	h := newHarness(t, false)
	id := h.wantOne("")

	var out struct {
		Count     int  `json:"count"`
		Truncated bool `json:"truncated"`
		Wants     []struct {
			DesiredItemID string `json:"desired_item_id"`
			Title         string `json:"title"`
			Profile       string `json:"quality_profile"`
			State         string `json:"state"`
		} `json:"wants"`
	}
	h.call("", "get_missing_content", "").structured(t, &out)

	if out.Count != 1 || out.Wants[0].DesiredItemID != id {
		t.Fatalf("missing content = %+v", out)
	}
	// The state distinguishes "nothing held" from "held but not good enough",
	// which have the same answer to "is this met" and need different action.
	if out.Wants[0].State != "MISSING" {
		t.Errorf("state = %q, want MISSING", out.Wants[0].State)
	}
	// The profile is named, not identified — an agent reporting this to a
	// person should be able to say "living-room" rather than a UUID.
	if out.Wants[0].Profile != "living-room" {
		t.Errorf("profile = %q, want the name", out.Wants[0].Profile)
	}
	if out.Truncated {
		t.Error("one row was reported as truncated")
	}
}

// The flagship. The reasons come back with their stable rule codes intact.
func TestExplainReleaseReturnsReasonsNotAVerdict(t *testing.T) {
	h := newHarness(t, false)

	var out struct {
		QualityProfile string `json:"quality_profile"`
		Selected       string `json:"selected"`
		Ranked         []struct {
			ID       string `json:"id"`
			Accepted bool   `json:"accepted"`
			Score    int    `json:"score"`
			Terminal bool   `json:"terminal"`
			Reasons  []struct {
				Rule    string `json:"rule"`
				Section string `json:"section"`
				Result  string `json:"result"`
				Detail  string `json:"detail"`
			} `json:"reasons"`
			RejectedBy []struct {
				Rule string `json:"rule"`
			} `json:"rejected_by"`
		} `json:"ranked"`
	}
	h.call("", "explain_release", `{"quality_profile":"living-room","releases":[
		{"id":"good","attributes":{"resolution":2160,"video_codec":"hevc"}},
		{"id":"small","attributes":{"resolution":480,"video_codec":"hevc"}}
	]}`).structured(t, &out)

	if len(out.Ranked) != 2 {
		t.Fatalf("%d results for 2 releases", len(out.Ranked))
	}
	if out.Selected != "good" {
		t.Errorf("selected = %q, want good", out.Selected)
	}

	best := out.Ranked[0]
	if !best.Accepted || best.Score != 20 || !best.Terminal {
		t.Errorf("best = %+v", best)
	}
	// Stable rule codes, in the profile's own order — not prose, and not
	// summarised. A client branching on prose breaks when the prose improves.
	if len(best.Reasons) != 3 {
		t.Fatalf("%d reasons for a 3-rule profile", len(best.Reasons))
	}
	for _, r := range best.Reasons {
		if !strings.Contains(r.Rule, ".") {
			t.Errorf("rule %q is not a stable <attribute>.<op> code", r.Rule)
		}
		if r.Detail == "" {
			t.Errorf("%s has a result and no explanation", r.Rule)
		}
	}

	worst := out.Ranked[1]
	if worst.Accepted {
		t.Error("a 480p release should fail a 1080p gate")
	}
	if len(worst.RejectedBy) == 0 || worst.RejectedBy[0].Rule != "resolution.gte" {
		t.Errorf("the rejection should name the gate: %+v", worst.RejectedBy)
	}
}

// An attribute left OUT is undetermined, not false. That is a different answer
// from a wrong one and sends a person somewhere different.
func TestExplainReleaseReportsUndeterminedRatherThanGuessing(t *testing.T) {
	h := newHarness(t, false)

	var out struct {
		Ranked []struct {
			Accepted bool `json:"accepted"`
			Reasons  []struct {
				Rule   string `json:"rule"`
				Result string `json:"result"`
				Detail string `json:"detail"`
			} `json:"reasons"`
		} `json:"ranked"`
	}
	// No resolution at all.
	h.call("", "explain_release", `{"quality_profile":"living-room","releases":[
		{"id":"unmeasured","attributes":{"video_codec":"hevc"}}]}`).structured(t, &out)

	if out.Ranked[0].Accepted {
		t.Error("a gate that cannot be shown to hold must not pass")
	}
	var found bool
	for _, r := range out.Ranked[0].Reasons {
		if r.Rule == "resolution.gte" {
			found = true
			if r.Result != "undetermined" {
				t.Errorf("result = %q, want undetermined", r.Result)
			}
			if !strings.Contains(r.Detail, "could not determine") {
				t.Errorf("detail = %q", r.Detail)
			}
		}
	}
	if !found {
		t.Error("no reason for the undetermined gate")
	}
}

// A misspelled attribute is refused rather than silently scored against
// nothing, which would produce an explanation that looks right.
func TestExplainReleaseRefusesAnUnknownAttribute(t *testing.T) {
	h := newHarness(t, false)
	resp := h.call("", "explain_release", `{"quality_profile":"living-room","releases":[
		{"id":"x","attributes":{"bitrate":5000}}]}`)
	if resp.Body.Error == nil {
		t.Fatal("an unknown attribute should be refused")
	}
	if !strings.Contains(resp.Body.Error.Message, "no attribute called") {
		t.Errorf("message = %q", resp.Body.Error.Message)
	}
}

// Nothing acceptable means nothing selected. An agent reading ranked[0] as
// "the answer" would otherwise recommend acquiring something the profile
// refuses.
func TestExplainReleaseSelectsNothingWhenNothingIsAcceptable(t *testing.T) {
	h := newHarness(t, false)
	var out struct {
		Selected string `json:"selected"`
		Ranked   []struct {
			Accepted bool `json:"accepted"`
		} `json:"ranked"`
	}
	h.call("", "explain_release", `{"quality_profile":"living-room","releases":[
		{"id":"a","attributes":{"resolution":480}},
		{"id":"b","attributes":{"resolution":720}}]}`).structured(t, &out)

	if out.Selected != "" {
		t.Errorf("selected = %q when nothing was acceptable", out.Selected)
	}
	for _, r := range out.Ranked {
		if r.Accepted {
			t.Error("a release below the gate was accepted")
		}
	}
}

func TestGetContentSatisfaction(t *testing.T) {
	h := newHarness(t, false)
	id := h.wantOne("")

	var out struct {
		State   string `json:"state"`
		Content struct {
			Satisfaction string `json:"satisfaction"`
			Assets       []any  `json:"assets"`
		} `json:"content"`
		Placement struct {
			Satisfaction string `json:"satisfaction"`
			Unproven     bool   `json:"unproven"`
		} `json:"placement"`
		Upgrade struct {
			Eligible bool   `json:"eligible"`
			Status   string `json:"status"`
		} `json:"upgrade"`
	}
	h.call("", "get_content_satisfaction",
		`{"desired_item_id":"`+id+`"}`).structured(t, &out)

	if out.State != "MISSING" {
		t.Errorf("state = %q, want MISSING", out.State)
	}
	if out.Content.Satisfaction != "not_satisfied" {
		t.Errorf("content = %q", out.Content.Satisfaction)
	}
	// §56's two axes are both answered, and the placement one says plainly it
	// has never run against a second peer.
	if !out.Placement.Unproven {
		t.Error("placement should declare itself unproven (ADR-0010)")
	}
	// §60's question is a third one: a want can be satisfied and still
	// improvable.
	if out.Upgrade.Status == "" {
		t.Error("the upgrade question was not answered")
	}
}

func TestGetPeerStatus(t *testing.T) {
	h := newHarness(t, false)
	var out struct {
		Count int    `json:"count"`
		Note  string `json:"note"`
		Peers []struct {
			Name   string `json:"name"`
			IsSelf bool   `json:"is_self"`
		} `json:"peers"`
	}
	h.call("", "get_peer_status", "").structured(t, &out)

	if out.Count != 1 || !out.Peers[0].IsSelf {
		t.Fatalf("peers = %+v", out)
	}
	// An agent seeing one peer would otherwise reasonably report a replication
	// problem that does not exist.
	if !strings.Contains(out.Note, "by design") {
		t.Error("the reply should say that one peer is the design, not a symptom")
	}
}

func TestGetReplicaStatus(t *testing.T) {
	h := newHarness(t, false)
	var out struct {
		Count    int `json:"count"`
		Replicas []struct {
			State  string `json:"state"`
			Counts bool   `json:"counts_for_placement"`
		} `json:"replicas"`
	}
	h.call("", "get_replica_status", `{"blob_hash":"`+blobHash+`"}`).structured(t, &out)

	if out.Count != 1 || !out.Replicas[0].Counts {
		t.Fatalf("replicas = %+v", out)
	}

	// A copy that is not verified is bytes somewhere, and NOT a replica for
	// the question §56 asks.
	h.exec(`UPDATE replicas SET state = 'corrupt' WHERE blob_hash = ?`, blobHash)
	h.call("", "get_replica_status", `{"blob_hash":"`+blobHash+`"}`).structured(t, &out)
	if out.Replicas[0].Counts {
		t.Error("a corrupt replica was counted for placement")
	}
}

func TestVerifyBlobQueuesRatherThanRuns(t *testing.T) {
	h := newHarness(t, false)

	var out struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	h.call("", "verify_blob", `{"blob_hash":"`+blobHash+`"}`).structured(t, &out)

	if out.JobID == "" || out.Status != "queued" {
		t.Fatalf("verify = %+v", out)
	}
	// Said plainly, because an agent that read this as "verified" would report
	// a clean bill of health nobody has established.
	if !strings.Contains(out.Note, "job") {
		t.Error("the reply should say the answer arrives on the job, not here")
	}

	// A mistyped hash is caught now rather than by a job that fails minutes
	// later somewhere the agent is not watching.
	resp := h.call("", "verify_blob", `{"blob_hash":"blake3:nope"}`)
	if resp.Body.Error == nil {
		t.Error("an unknown blob should be refused at call time")
	}
}

// An agent that misspelled an argument finds out, rather than getting a
// cheerful empty result it cannot learn from.
func TestUnknownArgumentsAreRefused(t *testing.T) {
	h := newHarness(t, false)
	resp := h.call("", "search_content", `{"quury":"arrival"}`)
	if resp.Body.Error == nil {
		t.Fatal("a misspelled argument should be refused")
	}
	if resp.Body.Error.Code != -32602 {
		t.Errorf("code = %d, want -32602", resp.Body.Error.Code)
	}
}

// Every tool result carries BOTH a text block every client can render and the
// structured value, so an agent gets prose it can quote and a shape it can
// branch on rather than having to parse the prose.
func TestResultsCarryTextAndStructure(t *testing.T) {
	h := newHarness(t, false)
	resp := h.call("", "get_peer_status", "")

	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(resp.Body.Result, &envelope); err != nil {
		t.Fatalf("result is not an MCP envelope: %s", resp.Body.Result)
	}
	if len(envelope.Content) == 0 || envelope.Content[0].Type != "text" {
		t.Fatalf("no text block: %+v", envelope.Content)
	}
	if len(envelope.Structured) == 0 {
		t.Fatal("no structuredContent")
	}
	// The two describe the same thing.
	if !strings.Contains(envelope.Content[0].Text, "peer-a") {
		t.Error("the text block does not carry the answer")
	}
}
