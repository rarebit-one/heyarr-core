package mcp_test

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// discover_content over the real JSON-RPC surface (#451). It shares
// resources.Discover with POST /api/v1/discover, so this asserts the MCP door
// reaches the same op — the "one intent, two doors" discipline search_content
// and follow_source are built on.

func TestDiscoverContentReturnsCandidates(t *testing.T) {
	reg := providers.New(nil)
	fake := providers.NewFake("fake-tvdb", providers.CapabilityMetadata).
		OfferDiscovery("the expanse",
			providers.DiscoveryCandidate{
				Title: "The Expanse", Year: 2015, ExternalID: "280619",
				Type: followed.TypeTVSeries, Overview: "A political thriller in space.",
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
	h := newHarness(t, false)
	resp := h.call("", "discover_content", `{"query":"  "}`)
	if resp.Body.Error == nil {
		t.Fatal("an empty discovery query must be refused")
	}
}

// A node with no metadata provider says so — actionably — rather than returning
// nothing. The default harness configures no providers, so this is that node.
func TestDiscoverContentWithoutAProviderSaysSo(t *testing.T) {
	h := newHarness(t, false)
	resp := h.call("", "discover_content", `{"query":"anything"}`)
	if resp.Body.Error == nil {
		t.Fatal("a node with no discovery provider must refuse, not answer empty")
	}
	if msg := resp.Body.Error.Message; msg == "" {
		t.Error("the refusal carries no message")
	}
}
