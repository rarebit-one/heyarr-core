package personalmcp_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/device/personalmcp"
)

func newStore(t *testing.T) *device.Store {
	t.Helper()
	store, err := device.NewStore(device.StoreOptions{
		Dir: filepath.Join(t.TempDir(), "config", "heyarr", "device"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// serve runs the requests through the real stdio transport and returns the raw
// transcript, so what is asserted is what a client would actually read.
func serve(t *testing.T, store *device.Store, requests ...string) string {
	t.Helper()
	var out bytes.Buffer
	srv, err := personalmcp.New(personalmcp.Options{
		Store:   store,
		Version: "test",
		Stdin:   strings.NewReader(strings.Join(requests, "\n") + "\n"),
		Stdout:  &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	return out.String()
}

// call is one tools/call request.
func call(id int, tool string, args map[string]any) string {
	encoded, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func decodeLines(t *testing.T, transcript string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(transcript), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("a line of the transcript is not JSON: %v\n%s", err, line)
		}
		out = append(out, msg)
	}
	return out
}

// TestTheToolSurfaceIsExactlyThese is the boundary assertion.
//
// Exact equality, not "contains": a verb added later — a sign, an enrol, a
// wrap — must fail this test rather than arrive unnoticed, which is the whole
// point of enumerating the surface (§72, §73, ADR-0032).
func TestTheToolSurfaceIsExactlyThese(t *testing.T) {
	want := []string{"device_generate", "device_list", "device_remove", "device_show"}

	srv, err := personalmcp.New(personalmcp.Options{Store: newStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("the registered tool surface is %v, want exactly %v", got, want)
	}

	// And the same list on the wire, because a registry a client cannot see is
	// not the contract.
	transcript := serve(t, newStore(t), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	msgs := decodeLines(t, transcript)
	if len(msgs) != 1 {
		t.Fatalf("tools/list produced %d messages, want 1", len(msgs))
	}
	result, ok := msgs[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list returned no result: %s", transcript)
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list returned no tools array: %s", transcript)
	}
	var names []string
	for _, entry := range tools {
		tool, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a tool descriptor is not an object: %v", entry)
		}
		name, _ := tool["name"].(string)
		names = append(names, name)
		if _, ok := tool["inputSchema"]; !ok {
			t.Errorf("%s publishes no inputSchema", name)
		}
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("tools/list published %v, want exactly %v", names, want)
	}
}

// TestInitializeIdentifiesItselfAsThePersonalMCP — an agent holding two
// connections must be able to tell which one it is talking to.
func TestInitializeIdentifiesItselfAsThePersonalMCP(t *testing.T) {
	transcript := serve(t, newStore(t), `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	msgs := decodeLines(t, transcript)
	result, ok := msgs[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize returned no result: %s", transcript)
	}
	info, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("initialize returned no serverInfo: %s", transcript)
	}
	if got, want := info["name"], "heyarr-personal"; got != want {
		t.Errorf("serverInfo.name = %v, want %v", got, want)
	}
	if got, want := result["protocolVersion"], "2025-06-18"; got != want {
		t.Errorf("protocolVersion = %v, want %v", got, want)
	}
	instructions, _ := result["instructions"].(string)
	if !strings.Contains(instructions, "does not authorise anything yet") {
		t.Errorf("the instructions do not tell an agent the key authorises nothing:\n%s", instructions)
	}
}

// TestAListResponseCarriesTheCaveatAsFields asserts the field, not the prose
// around it.
func TestAListResponseCarriesTheCaveatAsFields(t *testing.T) {
	store := newStore(t)
	transcript := serve(t, store,
		call(1, "device_generate", map[string]any{"name": "laptop"}),
		call(2, "device_list", map[string]any{}),
	)
	msgs := decodeLines(t, transcript)
	if len(msgs) != 2 {
		t.Fatalf("got %d replies, want 2:\n%s", len(msgs), transcript)
	}

	structured := func(msg map[string]any) map[string]any {
		t.Helper()
		result, ok := msg["result"].(map[string]any)
		if !ok {
			t.Fatalf("no result in %v", msg)
		}
		sc, ok := result["structuredContent"].(map[string]any)
		if !ok {
			t.Fatalf("no structuredContent in %v", result)
		}
		return sc
	}

	generated, ok := structured(msgs[0])["device"].(map[string]any)
	if !ok {
		t.Fatalf("device_generate returned no device: %s", transcript)
	}
	for _, tc := range []struct {
		field string
		want  any
	}{
		{"enrolment_status", "not_enrolled"},
		{"unproven", true},
		{"authorises", device.NotYetAuthorisingFor(device.CommandHint)},
		{"algorithm", "ed25519"},
	} {
		if got := generated[tc.field]; got != tc.want {
			t.Errorf("device_generate: %s = %v, want %v", tc.field, got, tc.want)
		}
	}

	listed := structured(msgs[1])
	devices, ok := listed["devices"].([]any)
	if !ok || len(devices) != 1 {
		t.Fatalf("device_list returned %v, want one device", listed["devices"])
	}
	first, ok := devices[0].(map[string]any)
	if !ok {
		t.Fatalf("the listed device is not an object: %v", devices[0])
	}
	if got, want := first["enrolment_status"], "not_enrolled"; got != want {
		t.Errorf("device_list: enrolment_status = %v, want %v", got, want)
	}
	if got := first["unproven"]; got != true {
		t.Errorf("device_list: unproven = %v, want true", got)
	}
	if got, want := first["public_key"], generated["public_key"]; got != want {
		t.Errorf("device_list reported public key %v, device_generate reported %v", got, want)
	}
	if got, want := listed["authorises"], device.NotYetAuthorisingFor(device.CommandHint); got != want {
		t.Errorf("device_list: authorises = %v, want the heyarr caveat", got)
	}
}

// TestNoResponseEverContainsKeyMaterial scans the transcript, rather than the
// code, for the bytes that must never leave the machine.
func TestNoResponseEverContainsKeyMaterial(t *testing.T) {
	store := newStore(t)
	dev, err := store.Generate("laptop", false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	seedHex := strings.TrimSpace(string(raw))
	seedHex = seedHex[strings.IndexByte(seedHex, ':')+1:]
	seed, err := hex.DecodeString(seedHex)
	if err != nil {
		t.Fatalf("the key file is not the shape this test assumes: %v", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)

	transcript := serve(t, store,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		call(3, "device_list", map[string]any{}),
		call(4, "device_show", map[string]any{"device_id": dev.ID}),
		call(5, "device_generate", map[string]any{"name": "again"}),
		call(6, "device_remove", map[string]any{"device_id": dev.ID}),
	)
	if strings.TrimSpace(transcript) == "" {
		t.Fatal("the transcript is empty, so this scan proves nothing")
	}

	for what, needle := range map[string]string{
		"the seed in hex":        seedHex,
		"the key file verbatim":  strings.TrimSpace(string(raw)),
		"the private key in hex": hex.EncodeToString(priv),
		"the seed as raw bytes":  string(seed),
	} {
		if needle == "" {
			t.Fatalf("the %s needle is empty, so this assertion proves nothing", what)
		}
		if strings.Contains(transcript, needle) {
			t.Errorf("%s appears in an MCP response:\n%s", what, transcript)
		}
	}
	// The public key must be there, or the scan above passed because nothing
	// interesting was in the transcript at all.
	if !strings.Contains(transcript, dev.PublicKeyString()) {
		t.Fatalf("the transcript does not contain the public key, so the scan proves nothing:\n%s", transcript)
	}
}

// TestRefusalsCarryStableReasons — one case each, with the machine-readable
// reason an agent branches on.
func TestRefusalsCarryStableReasons(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, store *device.Store) string
		want    string
	}{
		{
			name: "a world-readable key file",
			arrange: func(t *testing.T, store *device.Store) string {
				t.Helper()
				if _, err := store.Generate("laptop", false); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(store.KeyPath(), 0o644); err != nil {
					t.Fatal(err)
				}
				return call(1, "device_list", map[string]any{})
			},
			want: "key_permissions",
		},
		{
			name: "a key file that is not a key",
			arrange: func(t *testing.T, store *device.Store) string {
				t.Helper()
				if _, err := store.Generate("laptop", false); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(store.KeyPath(), []byte("nope\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return call(1, "device_list", map[string]any{})
			},
			want: "malformed_key",
		},
		{
			name: "removing a device that does not exist",
			arrange: func(t *testing.T, store *device.Store) string {
				t.Helper()
				if _, err := store.Generate("laptop", false); err != nil {
					t.Fatal(err)
				}
				return call(1, "device_remove", map[string]any{"device_id": "01920000-0000-7000-8000-000000000000"})
			},
			want: "unknown_device",
		},
		{
			name: "regenerating without force",
			arrange: func(t *testing.T, store *device.Store) string {
				t.Helper()
				if _, err := store.Generate("laptop", false); err != nil {
					t.Fatal(err)
				}
				return call(1, "device_generate", map[string]any{"name": "laptop"})
			},
			want: "device_exists",
		},
		{
			name: "showing a device on a machine that has none",
			arrange: func(t *testing.T, _ *device.Store) string {
				t.Helper()
				return call(1, "device_show", map[string]any{})
			},
			want: "no_device",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			request := tt.arrange(t, store)
			msgs := decodeLines(t, serve(t, store, request))
			if len(msgs) != 1 {
				t.Fatalf("got %d replies, want 1", len(msgs))
			}
			rpcErr, ok := msgs[0]["error"].(map[string]any)
			if !ok {
				t.Fatalf("the call succeeded, want a refusal with reason %q: %v", tt.want, msgs[0])
			}
			data, ok := rpcErr["data"].(map[string]any)
			if !ok {
				t.Fatalf("the refusal carries no data: %v", rpcErr)
			}
			if got := data["reason"]; got != tt.want {
				t.Errorf("reason = %v, want %v — an agent branching on the message breaks "+
					"when the message improves", got, tt.want)
			}
		})
	}
}

// TestANotificationIsNotAnswered — JSON-RPC says it MUST NOT be, and some
// clients treat a reply to one as fatal.
func TestANotificationIsNotAnswered(t *testing.T) {
	transcript := serve(t, newStore(t),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
	)
	msgs := decodeLines(t, transcript)
	if len(msgs) != 1 {
		t.Fatalf("got %d replies to a notification plus a ping, want 1:\n%s", len(msgs), transcript)
	}
	if fmt.Sprint(msgs[0]["id"]) != "7" {
		t.Errorf("the one reply is to id %v, want the ping (7)", msgs[0]["id"])
	}
}

// TestMalformedAndUnknownMessages — the protocol errors, each distinct.
func TestMalformedAndUnknownMessages(t *testing.T) {
	tests := []struct {
		name    string
		request string
		want    float64
	}{
		{name: "not JSON", request: `{`, want: -32700},
		{name: "a different JSON-RPC version", request: `{"jsonrpc":"1.0","id":1,"method":"ping"}`, want: -32600},
		{name: "an unknown method", request: `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, want: -32601},
		{
			name:    "an unknown tool",
			request: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sign_release"}}`,
			want:    -32601,
		},
		{
			name:    "an unknown argument",
			request: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"device_list","arguments":{"all":true}}}`,
			want:    -32602,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := decodeLines(t, serve(t, newStore(t), tt.request))
			if len(msgs) != 1 {
				t.Fatalf("got %d replies, want 1", len(msgs))
			}
			rpcErr, ok := msgs[0]["error"].(map[string]any)
			if !ok {
				t.Fatalf("the message was accepted, want an error: %v", msgs[0])
			}
			if got := rpcErr["code"]; got != tt.want {
				t.Errorf("code = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTheServerNeedsAStore — an MCP server with no store would answer every
// call with an internal error rather than refusing to start.
func TestTheServerNeedsAStore(t *testing.T) {
	if _, err := personalmcp.New(personalmcp.Options{}); err == nil {
		t.Error("a server with no device store was constructed")
	}
}
