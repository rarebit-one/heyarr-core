//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package mcp_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// JSON-RPC 2.0, the parts a client will actually exercise.

func TestProtocolRefusals(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
	}{
		{"not JSON at all", `{`, -32700},
		{"a different JSON-RPC version", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, -32600},
		{"a method that does not exist", `{"jsonrpc":"2.0","id":1,"method":"nope"}`, -32601},
		{
			"tools/call with no name",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, -32602,
		},
		{
			"tools/call with params that are not an object",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"nope"}`, -32602,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			resp := h.rpc("", tc.body)
			if resp.Body.Error == nil {
				t.Fatalf("expected a refusal, got: %s", resp.Raw)
			}
			if resp.Body.Error.Code != tc.code {
				t.Errorf("code = %d, want %d (%s)",
					resp.Body.Error.Code, tc.code, resp.Body.Error.Message)
			}
		})
	}
}

// A request with no id is a notification and MUST NOT be answered. Some
// clients treat a reply to one as a protocol violation, so this is not
// pedantry.
func TestANotificationIsNotAnswered(t *testing.T) {
	h := newHarness(t, false)
	resp := h.rpc("", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	if resp.Status != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.Status)
	}
	if len(resp.Raw) != 0 {
		t.Errorf("a notification was answered with a body: %s", resp.Raw)
	}
}

// The id round-trips exactly, including its JSON type. A client correlating
// replies by id would mismatch every one if a string became a number.
func TestTheRequestIDRoundTrips(t *testing.T) {
	h := newHarness(t, false)
	for _, id := range []string{`1`, `"abc"`, `42`} {
		resp := h.rpc("", `{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`)
		if string(resp.Body.ID) != id {
			t.Errorf("id %s came back as %s", id, resp.Body.ID)
		}
		if resp.Body.JSONRPC != "2.0" {
			t.Errorf("jsonrpc = %q", resp.Body.JSONRPC)
		}
	}
}

// A tool failure is an error INSIDE the JSON-RPC envelope, not an HTTP status.
// A client that saw a 400 would treat the transport as broken and reconnect.
func TestAToolFailureIsStill200(t *testing.T) {
	h := newHarness(t, false)
	resp := h.call("", "verify_blob", `{}`)

	if resp.Status != http.StatusOK {
		t.Errorf("status = %d, want 200 — a tool failure is a JSON-RPC error, not a "+
			"transport failure", resp.Status)
	}
	if resp.Body.Error == nil {
		t.Fatal("expected an error in the envelope")
	}
}

// The tool schemas are a golden file.
//
// A schema is an interface contract with the same permanence as an endpoint —
// more, because an agent was built against the field names and there is no
// deprecation header an agent reads. So a change to one has to show up in a
// reviewable diff rather than in an agent's behaviour six months later.
func TestToolSurfaceGolden(t *testing.T) {
	h := newHarness(t, false)
	resp := h.rpc("", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	var result any
	if err := json.Unmarshal(resp.Body.Result, &result); err != nil {
		t.Fatal(err)
	}
	rendered, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, filepath.Join("testdata", "tools_list.json"), append(rendered, '\n'))
}

// The deferrals are golden too, so removing one — which is what shipping a
// verb looks like — is a deliberate, reviewable change rather than a silent
// one.
func TestDeferralsGolden(t *testing.T) {
	h := newHarness(t, false)

	type entry struct {
		Verb string          `json:"verb"`
		Data json.RawMessage `json:"data"`
	}
	var entries []entry
	// sync_peer is NOT here any more, and its absence is the point: it was
	// deferred because the peer model held one peer, and Milestone 4 shipped
	// it. Removing a name from this list is what shipping a verb looks like.
	for _, verb := range []string{
		"acquire_release", "play_content", "search_releases",
		"transfer_playback",
	} {
		resp := h.call("", verb, "{}")
		if resp.Body.Error == nil {
			t.Fatalf("%s is no longer deferred — update this golden deliberately", verb)
		}
		entries = append(entries, entry{Verb: verb, Data: resp.Body.Error.Data})
	}
	rendered, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, filepath.Join("testdata", "deferred.json"), append(rendered, '\n'))
}
