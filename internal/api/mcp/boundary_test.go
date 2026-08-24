//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package mcp_test

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/mcp"
	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// The two boundaries this package exists inside: ADR-0011's scopes and §72's
// personal-state wall.
//
// Both are asserted by ENUMERATING the registered surface rather than by
// checking the cases someone remembered. A happy-path test catches neither.

// An MCP session is not a new trust domain (ADR-0011). A read token that could
// call a mutating verb would be a read token that can write, and nothing about
// a successful call would ever reveal it.
func TestAReadTokenCannotCallAMutatingTool(t *testing.T) {
	h := newHarness(t, true)
	read := h.mint("reader", auth.ScopeRead)

	for _, tool := range []struct{ name, args string }{
		{"want_content", `{"work_id":"` + workID + `","quality_profile":"living-room"}`},
		{"monitor_content", `{"desired_item_id":"whatever","monitor":false}`},
		{"verify_blob", `{"blob_hash":"` + blobHash + `"}`},
	} {
		t.Run(tool.name, func(t *testing.T) {
			resp := h.call(read, tool.name, tool.args)
			if resp.Body.Error == nil {
				t.Fatalf("a read token called %s and was allowed: %s", tool.name, resp.Raw)
			}
			// Forbidden, NOT invalid-params. An agent that cannot tell "you may
			// not do this" from "you asked wrongly" retries the wrong one
			// forever.
			if resp.Body.Error.Code != -32001 {
				t.Errorf("code = %d, want -32001 (forbidden)", resp.Body.Error.Code)
			}
			// And it names the scope, so the agent can say what is needed
			// rather than only that something failed.
			if !strings.Contains(resp.Body.Error.Message, "write") {
				t.Errorf("the refusal should name the scope required; got %q",
					resp.Body.Error.Message)
			}
			if !strings.Contains(string(resp.Body.Error.Data), tool.name) {
				t.Errorf("the refusal should name the tool; got %s", resp.Body.Error.Data)
			}
		})
	}
}

// The same tools, with a write token, are allowed — otherwise the test above
// would pass against a server that refused everything.
func TestAWriteTokenCanCallAMutatingTool(t *testing.T) {
	h := newHarness(t, true)
	write := h.mint("writer", auth.ScopeRead, auth.ScopeWrite)

	resp := h.call(write, "want_content",
		`{"work_id":"`+workID+`","quality_profile":"living-room"}`)
	if resp.Body.Error != nil {
		t.Fatalf("a write token was refused want_content: %d %s",
			resp.Body.Error.Code, resp.Body.Error.Message)
	}
}

// Reads need only read, which is what makes an agent with a read-only
// credential useful rather than inert.
func TestAReadTokenCanCallEveryReadOnlyTool(t *testing.T) {
	h := newHarness(t, true)
	read := h.mint("reader", auth.ScopeRead)

	args := map[string]string{
		"search_content":           `{"query":"arrival"}`,
		"get_replica_status":       `{"blob_hash":"` + blobHash + `"}`,
		"explain_release":          `{"quality_profile":"living-room","releases":[{"id":"a","attributes":{"resolution":2160}}]}`,
		"get_content_satisfaction": "",
	}

	for _, tool := range h.server.Tools() {
		if !tool.ReadOnly {
			continue
		}
		if tool.Name == "get_content_satisfaction" {
			// Needs a want to exist, and creating one needs write. Covered by
			// its own test rather than skipped silently.
			continue
		}
		t.Run(tool.Name, func(t *testing.T) {
			resp := h.call(read, tool.Name, args[tool.Name])
			if resp.Body.Error != nil && resp.Body.Error.Code == -32001 {
				t.Errorf("%s is read-only but refused a read token", tool.Name)
			}
		})
	}
}

// §72: controller-side MCP cannot decrypt user artifacts, and nothing here may
// expose personal state.
//
// Enumerated over the REGISTERED surface rather than inspected, so it still
// holds when someone adds a convenient tool in Milestone 9 — which is exactly
// when this boundary is most likely to be crossed by accident.
func TestNoToolTouchesPersonalState(t *testing.T) {
	h := newHarness(t, false)

	// §37–§47's vocabulary. A tool whose NAME or DESCRIPTION reaches for any of
	// these is either exposing personal state or about to.
	forbidden := []string{
		"playlist", "rating", "annotation", "bookmark",
		"reading position", "reading progress", "history",
		"watch history", "personal state", "private state",
	}

	tools := h.server.Tools()
	if len(tools) == 0 {
		t.Fatal("no tools are registered, so this test asserts nothing")
	}
	for _, tool := range tools {
		haystack := strings.ToLower(tool.Name + " " + tool.Title + " " + tool.Description)
		for _, word := range forbidden {
			if strings.Contains(haystack, word) {
				t.Errorf("%s reaches for personal state (%q), which controller-side MCP "+
					"cannot decrypt (§72) — it belongs to a Personal MCP (§73, M9)",
					tool.Name, word)
			}
		}
	}
}

// Every tool declares a coherent scope, and the read-only flag agrees with it.
//
// The registry panics on a contradiction at wiring time, so this asserts the
// surface that actually got registered rather than trusting that nobody
// bypassed it.
func TestEveryToolDeclaresACoherentScope(t *testing.T) {
	h := newHarness(t, false)

	for _, tool := range h.server.Tools() {
		if _, err := auth.ParseScope(string(tool.Scope)); err != nil {
			t.Errorf("%s declares no valid scope: %v", tool.Name, err)
		}
		// A read-only tool demanding `write` would make an MCP client's
		// confirmation prompt lie in the safe direction; a mutating tool marked
		// read-only would make it lie in the dangerous one.
		if tool.ReadOnly && tool.Scope != auth.ScopeRead {
			t.Errorf("%s is read-only but demands %s", tool.Name, tool.Scope)
		}
		if !tool.ReadOnly && tool.Scope == auth.ScopeRead {
			t.Errorf("%s mutates but demands only read", tool.Name)
		}
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("%s has no description — an agent choosing between tools reads it",
				tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("%s has no input schema", tool.Name)
		}
	}
}

// Every verb §71 lists is either shipped or explicitly deferred with the
// milestone that brings it. Nothing is silently missing.
func TestEverySpecVerbIsShippedOrDeferred(t *testing.T) {
	h := newHarness(t, false)

	// §71's list, verbatim.
	spec := []string{
		"search_content", "want_content", "monitor_content",
		"search_releases", "explain_release", "acquire_release",
		"get_peer_status", "get_replica_status", "sync_peer", "verify_blob",
		"play_content", "transfer_playback",
		"get_missing_content", "get_upgrade_candidates",
	}

	shipped := map[string]bool{}
	for _, name := range h.server.Names() {
		shipped[name] = true
	}

	for _, verb := range spec {
		t.Run(verb, func(t *testing.T) {
			if shipped[verb] {
				return
			}
			// Not shipped, so asking for it must explain itself rather than
			// look like a typo.
			resp := h.call("", verb, "{}")
			if resp.Body.Error == nil {
				t.Fatalf("%s is neither shipped nor refused", verb)
			}
			if resp.Body.Error.Code != -32601 {
				t.Errorf("code = %d, want -32601", resp.Body.Error.Code)
			}
			data := string(resp.Body.Error.Data)
			if !strings.Contains(data, "milestone") {
				t.Errorf("%s is deferred but names no milestone: %s", verb, data)
			}
			if !strings.Contains(data, "reason") {
				t.Errorf("%s is deferred but gives no reason: %s", verb, data)
			}
		})
	}
}

// No tool answers "not implemented". A stub is worse than an absence: it is a
// published vocabulary with a hole in it, which is what ADR-0019 waited to
// avoid.
func TestNoToolIsAStub(t *testing.T) {
	h := newHarness(t, false)

	for _, tool := range h.server.Tools() {
		if _, deferred := deferredNames()[tool.Name]; deferred {
			t.Errorf("%s is registered AND recorded as deferred — it must be one or "+
				"the other", tool.Name)
		}
		if strings.Contains(strings.ToLower(tool.Description), "not implemented") {
			t.Errorf("%s describes itself as unimplemented", tool.Name)
		}
	}
}

// deferredNames mirrors the deferral list from outside the package, so the two
// cannot silently agree by sharing a variable.
// deferredNames is DERIVED from the production table rather than repeating it.
//
// It used to be a hand-written copy of the same four names, which is the very
// thing #226 is about one layer down: two lists of what is deferred, nothing
// keeping them in step, and the test comparing the code against a stale copy of
// itself. When search_releases and acquire_release shipped, the production
// table lost them and this one did not — so the test that exists to catch a
// stub instead reported the two verbs as both registered and deferred.
//
// Derived, it cannot disagree. What it asserts is unchanged and still real: no
// REGISTERED tool may also be recorded as deferred.
func deferredNames() map[string]bool {
	return mcp.DeferredVerbs()
}

// An unknown name that is NOT a §71 verb gets no milestone. An agent that
// mistyped should not be told to wait for something that is coming.
func TestAMistypedToolIsNotReportedAsDeferred(t *testing.T) {
	h := newHarness(t, false)
	resp := h.call("", "want_contnet", "{}")
	if resp.Body.Error == nil {
		t.Fatal("a mistyped tool should be refused")
	}
	if resp.Body.Error.Code != -32601 {
		t.Errorf("code = %d, want -32601", resp.Body.Error.Code)
	}
	if strings.Contains(string(resp.Body.Error.Data), "milestone") {
		t.Error("a typo was reported as a deferred feature, so an agent would wait " +
			"for something that is never coming")
	}
}
