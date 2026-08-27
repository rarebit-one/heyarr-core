package personalmcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/device/personalmcp"
)

// fakeReader stands in for the device-side decrypt path: it returns a playlist a
// real reader would produce by unwrapping and decrypting on the device.
type fakeReader struct{ items map[string][]string }

func (f fakeReader) Playlist(spaceID string) ([]string, error) { return f.items[spaceID], nil }

// TestPersonalStateToolsAppearOnlyWhenAReaderIsWired: no reader, the surface is
// the M8 device-key shell; with a reader, it also exposes personal_playlist.
func TestPersonalStateToolsAppearOnlyWhenAReaderIsWired(t *testing.T) {
	bare, err := personalmcp.New(personalmcp.Options{Store: newStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	if contains(bare.Names(), "personal_playlist") {
		t.Fatal("personal_playlist is exposed with no reader wired")
	}

	withReader, err := personalmcp.New(personalmcp.Options{
		Store:         newStore(t),
		PersonalState: fakeReader{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(withReader.Names(), "personal_playlist") {
		t.Fatalf("personal_playlist is not exposed with a reader wired: %v", withReader.Names())
	}
}

// TestPersonalPlaylistReturnsTheDecryptedItems drives the tool over the real
// stdio transport and asserts it returns the plaintext playlist — the state the
// device alone can produce (§73).
func TestPersonalPlaylistReturnsTheDecryptedItems(t *testing.T) {
	reader := fakeReader{items: map[string][]string{"space-1": {"midnight-jazz", "morning-coffee"}}}
	var out bytes.Buffer
	srv, err := personalmcp.New(personalmcp.Options{
		Store:         newStore(t),
		PersonalState: reader,
		Stdin: strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"personal_playlist","arguments":{"space_id":"space-1"}}}` + "\n"),
		Stdout: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}

	// The tool result carries the decrypted items.
	if !strings.Contains(out.String(), "midnight-jazz") || !strings.Contains(out.String(), "morning-coffee") {
		t.Fatalf("the Personal MCP did not return the decrypted playlist:\n%s", out.String())
	}
	// Sanity: it is valid JSON-RPC.
	var msg map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &msg); err != nil {
		t.Fatalf("the response is not JSON: %v\n%s", err, out.String())
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
