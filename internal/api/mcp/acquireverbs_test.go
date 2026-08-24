//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package mcp_test

import (
	"strings"
	"testing"
)

// search_releases QUEUES a search rather than performing one.
//
// A search is a job (invariant 4) that a different process may run, and an
// indexer can take thirty seconds to refuse — so an agent gets a job back and
// reads the want afterwards. A verb that blocked would make an agent's turn
// hostage to somebody else's tracker.
func TestSearchReleasesQueuesAJobRatherThanBlocking(t *testing.T) {
	h := newHarness(t, false)
	id := h.wantOne("")

	var out struct {
		DesiredItemID string `json:"desired_item_id"`
		JobID         string `json:"job_id"`
		Status        string `json:"status"`
	}
	h.call("", "search_releases", `{"desired_item_id":"`+id+`"}`).structured(t, &out)

	if out.DesiredItemID != id {
		t.Errorf("desired_item_id = %q, want %q", out.DesiredItemID, id)
	}
	if out.JobID == "" {
		t.Error("no job id, so an agent cannot tell the search was accepted")
	}
	if out.Status != "queued" {
		t.Errorf("status = %q, want queued — the verb must not imply it has answers", out.Status)
	}
}

// Scope enforcement is NOT re-tested here on purpose.
//
// boundary_test.go's TestAReadTokenCannotCallAMutatingTool ENUMERATES every
// registered tool and refuses a read token for each mutating one, so both verbs
// are covered by it the moment they are registered — and by a stronger test
// than a per-verb one, because it cannot be forgotten for the next verb.

// acquire_release refuses a candidate the quality profile rejected, and the
// refusal names what to do instead.
//
// An agent that could override a gate would turn the operator's own statement
// of what is acceptable into a suggestion. The candidate here does not exist
// for this want at all, which is the same refusal path a superseded search
// produces.
func TestAcquireReleaseRefusesACandidateThisWantDoesNotHave(t *testing.T) {
	h := newHarness(t, false)
	id := h.wantOne("")

	resp := h.call("", "acquire_release",
		`{"desired_item_id":"`+id+`","candidate_id":"not-a-candidate"}`)
	if resp.Body.Error == nil {
		t.Fatal("acquiring a candidate the want does not have was accepted")
	}
	if !strings.Contains(strings.ToLower(resp.Body.Error.Message), "candidate") {
		t.Errorf("the refusal does not mention the candidate: %q", resp.Body.Error.Message)
	}
}

// Both arguments are required, and the refusal says which is missing.
//
// A candidate id with no want is ambiguous — the same release may be a
// candidate for several wants — and a want with no candidate would make this
// "acquire something", which is what the scorer is for.
func TestAcquireReleaseNeedsBothAWantAndACandidate(t *testing.T) {
	h := newHarness(t, false)
	id := h.wantOne("")

	for _, tc := range []struct {
		name, args, missing string
	}{
		{"no candidate", `{"desired_item_id":"` + id + `"}`, "candidate_id"},
		{"no want", `{"candidate_id":"c1"}`, "desired_item_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.call("", "acquire_release", tc.args)
			if resp.Body.Error == nil {
				t.Fatal("accepted with an argument missing")
			}
			if !strings.Contains(resp.Body.Error.Message, tc.missing) {
				t.Errorf("the refusal does not name %s: %q", tc.missing, resp.Body.Error.Message)
			}
		})
	}
}

// Neither verb is reported as deferred any more, and an agent asking for one
// gets the tool rather than a milestone to wait for.
//
// This is #226's actual complaint: an entry that is WRONG produces an agent
// that waits for something which already shipped, which is worse than the
// missing tool the mechanism exists to avoid.
func TestTheShippedAcquisitionVerbsAreNotStillDeferred(t *testing.T) {
	h := newHarness(t, false)
	deferred := deferredNames()

	for _, verb := range []string{"search_releases", "acquire_release"} {
		if deferred[verb] {
			t.Errorf("%s is shipped and still recorded as deferred", verb)
		}
		var found bool
		for _, tool := range h.server.Tools() {
			if tool.Name == verb {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is not registered", verb)
		}
	}
}
