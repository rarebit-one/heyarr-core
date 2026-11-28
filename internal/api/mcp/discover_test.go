package mcp_test

import (
	"fmt"
	"github.com/rarebit-one/heyarr-core/internal/providers/musicbrainz"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// discover_content over the real JSON-RPC surface (#451). It shares
// resources.Discover with POST /api/v1/discover, so this asserts the MCP door
// reaches the same op — the "one intent, two doors" discipline search_content
// and follow_source are built on.

func TestDiscoverContentReturnsCandidates(t *testing.T) {
	t.Parallel()
	reg := providers.New(nil)
	fake := providers.NewFake("fake-tvdb", providers.CapabilityMetadata).
		OfferDiscovery("the expanse",
			providers.DiscoveryCandidate{
				Title: "The Expanse", Year: 2015, ExternalID: "280619", Source: "tvdb",
				Type: string(followed.TypeTVSeries), Overview: "A political thriller in space.",
			})
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, false, reg)

	var out struct {
		Count   int `json:"count"`
		Results []struct {
			Title  string `json:"title"`
			Year   int    `json:"year"`
			Type   string `json:"type"`
			TVDBID string `json:"tvdb_id"`
		} `json:"results"`
	}
	h.call("", "discover_content", `{"query":"the expanse"}`).structured(t, &out)
	if out.Count != 1 || len(out.Results) != 1 {
		t.Fatalf("discover_content = %+v", out)
	}
	got := out.Results[0]
	if got.Title != "The Expanse" || got.TVDBID != "280619" || got.Type != "tv_series" {
		t.Errorf("candidate = %+v", got)
	}
}

// A query with nothing to search on is an invalid-params refusal, not a result.
func TestDiscoverContentRefusesEmptyQuery(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false)
	resp := h.call("", "discover_content", `{"query":"  "}`)
	if resp.Body.Error == nil {
		t.Fatal("an empty discovery query must be refused")
	}
}

// A node with no metadata provider says so — actionably — rather than returning
// nothing. The default harness configures no providers, so this is that node.
func TestDiscoverContentWithoutAProviderSaysSo(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false)
	resp := h.call("", "discover_content", `{"query":"anything"}`)
	if resp.Body.Error == nil {
		t.Fatal("a node with no discovery provider must refuse, not answer empty")
	}
	if msg := resp.Body.Error.Message; msg == "" {
		t.Error("the refusal carries no message")
	}
}

func TestDiscoverContentRecording(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/recording/" || !strings.Contains(r.URL.Query().Get("query"), `artist:"Cyndi Lauper"`) {
			t.Errorf("unexpected upstream query: %s", r.URL.String())
		}
		fmt.Fprint(w, `{"recordings":[{"id":"12345678-1234-4234-8234-123456789abc","title":"Time After Time","disambiguation":"live","artist-credit":[{"name":"Cyndi Lauper","artist":{"id":"11111111-1111-4111-8111-111111111111","name":"Cyndi Lauper"}}]}]}`)
	}))
	defer upstream.Close()
	reg := providers.New(nil)
	mb, err := musicbrainz.New(musicbrainz.Options{Name: "mb", Endpoint: upstream.URL, MinInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(mb); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, false, reg)
	var out struct {
		Count   int `json:"count"`
		Results []struct {
			RecordingID    string `json:"recording_id"`
			Disambiguation string `json:"disambiguation"`
			Artists        []struct {
				Name string `json:"name"`
			} `json:"artists"`
		} `json:"results"`
	}
	h.call("", "discover_content", `{"query":"Time After Time","artist":"Cyndi Lauper"}`).structured(t, &out)
	if out.Count != 1 || out.Results[0].RecordingID != "12345678-1234-4234-8234-123456789abc" || out.Results[0].Disambiguation != "live" || len(out.Results[0].Artists) != 1 || out.Results[0].Artists[0].Name != "Cyndi Lauper" {
		t.Fatalf("recording discovery = %+v", out)
	}
}
