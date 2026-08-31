package personalmcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/device/personalmcp"
)

// fakeReader stands in for the device-side decrypt path: it returns the state a
// real reader would produce by unwrapping and decrypting on the device.
type fakeReader struct {
	items   map[string][]string
	starred map[string][]string
	history map[string]personalmcp.PlayHistory
	readpos map[string][]personalmcp.ReadingPosition
}

func (f fakeReader) Playlist(spaceID string) ([]string, error) { return f.items[spaceID], nil }
func (f fakeReader) Starred(spaceID string) ([]string, error)  { return f.starred[spaceID], nil }
func (f fakeReader) History(spaceID string) (personalmcp.PlayHistory, error) {
	return f.history[spaceID], nil
}

func (f fakeReader) ReadingPositions(spaceID string) ([]personalmcp.ReadingPosition, error) {
	return f.readpos[spaceID], nil
}

// personalStateTools is every read verb wired when a reader is present.
var personalStateTools = []string{
	"personal_playlist", "personal_starred", "personal_history", "personal_reading_position",
}

// TestPersonalStateToolsAppearOnlyWhenAReaderIsWired: no reader, the surface is
// the M8 device-key shell; with a reader, it exposes every personal-state verb.
func TestPersonalStateToolsAppearOnlyWhenAReaderIsWired(t *testing.T) {
	bare, err := personalmcp.New(personalmcp.Options{Store: newStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range personalStateTools {
		if contains(bare.Names(), name) {
			t.Fatalf("%s is exposed with no reader wired", name)
		}
	}

	withReader, err := personalmcp.New(personalmcp.Options{
		Store:         newStore(t),
		PersonalState: fakeReader{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range personalStateTools {
		if !contains(withReader.Names(), name) {
			t.Fatalf("%s is not exposed with a reader wired: %v", name, withReader.Names())
		}
	}
}

// callTool drives one tools/call over the real stdio transport and returns the
// raw reply, so each read verb is exercised exactly as an agent would reach it.
func callTool(t *testing.T, reader personalmcp.PersonalStateReader, name, spaceID string) string {
	t.Helper()
	var out bytes.Buffer
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name +
		`","arguments":{"space_id":"` + spaceID + `"}}}` + "\n"
	srv, err := personalmcp.New(personalmcp.Options{
		Store: newStore(t), PersonalState: reader,
		Stdin: strings.NewReader(req), Stdout: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &msg); err != nil {
		t.Fatalf("the response is not JSON-RPC: %v\n%s", err, out.String())
	}
	return out.String()
}

// TestPersonalStarredReturnsTheDecryptedItems: the starred verb surfaces the
// device-decrypted item ids.
func TestPersonalStarredReturnsTheDecryptedItems(t *testing.T) {
	reader := fakeReader{starred: map[string][]string{"space-1": {"tr:hot", "tr:cool"}}}
	got := callTool(t, reader, "personal_starred", "space-1")
	if !strings.Contains(got, "tr:hot") || !strings.Contains(got, "tr:cool") {
		t.Fatalf("personal_starred did not return the decrypted stars:\n%s", got)
	}
}

// TestPersonalHistoryReturnsTheDecryptedViews: the history verb surfaces recent,
// frequent (with counts) and now-playing.
func TestPersonalHistoryReturnsTheDecryptedViews(t *testing.T) {
	reader := fakeReader{history: map[string]personalmcp.PlayHistory{
		"space-1": {
			Recent:     []string{"tr:new", "tr:old"},
			Frequent:   []personalmcp.ItemCount{{ID: "tr:old", Count: 3}},
			NowPlaying: "tr:new",
		},
	}}
	got := callTool(t, reader, "personal_history", "space-1")
	// The reply carries both a prose (escaped-JSON) rendering and a
	// structuredContent object; assert on the structured half, which is stable.
	var reply struct {
		Result struct {
			Structured struct {
				Recent   []string `json:"recent"`
				Frequent []struct {
					ID    string `json:"id"`
					Count int    `json:"count"`
				} `json:"frequent"`
				NowPlaying string `json:"now_playing"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &reply); err != nil {
		t.Fatalf("history reply not JSON: %v\n%s", err, got)
	}
	s := reply.Result.Structured
	if strings.Join(s.Recent, ",") != "tr:new,tr:old" {
		t.Errorf("recent = %v, want [tr:new tr:old]", s.Recent)
	}
	if len(s.Frequent) != 1 || s.Frequent[0].ID != "tr:old" || s.Frequent[0].Count != 3 {
		t.Errorf("frequent = %+v, want one tr:old count 3", s.Frequent)
	}
	if s.NowPlaying != "tr:new" {
		t.Errorf("now_playing = %q, want tr:new", s.NowPlaying)
	}
}

// TestPersonalReadingPositionReturnsTheDecryptedPositions: the reading-position
// verb surfaces each publication's opaque locator.
func TestPersonalReadingPositionReturnsTheDecryptedPositions(t *testing.T) {
	reader := fakeReader{readpos: map[string][]personalmcp.ReadingPosition{
		"space-1": {{PubID: "pub:dune", Position: "epubcfi(/6/14)"}},
	}}
	got := callTool(t, reader, "personal_reading_position", "space-1")
	if !strings.Contains(got, "pub:dune") || !strings.Contains(got, "epubcfi(/6/14)") {
		t.Fatalf("personal_reading_position did not return the decrypted position:\n%s", got)
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
