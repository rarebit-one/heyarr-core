//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Discovery search over the real HTTP surface (§55, M12, #451) — the
// "not-yet-in-library" door beside /search. The op and its REST shell are
// exercised together, because the seam a client consumes is the route.

type discoveryResult struct {
	Title    string `json:"title"`
	Year     int    `json:"year"`
	Type     string `json:"type"`
	TVDBID   string `json:"tvdb_id"`
	Overview string `json:"overview"`
}

func discover(h *harness, body string) *http.Response {
	return h.doStable(http.MethodPost, "/api/v1/discover", strings.NewReader(body))
}

// A discovery search asks the metadata provider, so it returns a candidate the
// library does NOT hold — with the tvdb_id a client follows in one step.
func TestDiscoverReturnsCandidatesNotInTheLibrary(t *testing.T) {
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
	h := newHarness(t, withProviders(reg)).seed()

	resp := discover(h, `{"query":"the expanse"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("discover = %d: %s", resp.StatusCode, h.body(resp))
	}
	var out struct {
		Results []discoveryResult `json:"results"`
	}
	if err := json.Unmarshal(h.body(resp), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %+v, want one candidate", out.Results)
	}
	got := out.Results[0]
	if got.Title != "The Expanse" || got.Year != 2015 {
		t.Errorf("candidate = %+v", got)
	}
	if got.TVDBID != "280619" {
		t.Errorf("tvdb_id = %q, want the id a follow acts on", got.TVDBID)
	}
	if got.Type != "tv_series" || got.Overview == "" {
		t.Errorf("candidate lost its type/overview: %+v", got)
	}
	if fake.Discoveries() != 1 {
		t.Errorf("the provider was asked %d times, want 1", fake.Discoveries())
	}
}

// A query with nothing to search on is refused — a 400, not an empty 200.
func TestDiscoverRefusesEmptyQuery(t *testing.T) {
	reg := providers.New(nil)
	if err := reg.Register(
		providers.NewFake("fake-tvdb", providers.CapabilityMetadata)); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, withProviders(reg)).seed()

	resp := discover(h, `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an empty discovery query = %d, want 400", resp.StatusCode)
	}
}

// A node with no provider that can discover says so with a 503 — the request was
// fine and nothing failed, there is simply nothing configured to answer. That is
// different from a 400 (caller's fault) and a 500 (server failure), and it must
// not read as "found nothing".
func TestDiscoverWithoutAProviderIs503(t *testing.T) {
	// A registry whose only provider is an indexer — a real "no metadata
	// provider can look content up" node, not merely an empty registry.
	reg := providers.New(nil)
	if err := reg.Register(
		providers.NewFake("an-indexer", providers.CapabilityIndexer)); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, withProviders(reg)).seed()

	resp := discover(h, `{"query":"anything"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("discover with no metadata provider = %d, want 503: %s",
			resp.StatusCode, h.body(resp))
	}
	// The refusal is actionable — it names what to configure — rather than a
	// bare status.
	if body := string(h.body(resp)); !strings.Contains(body, "metadata provider") {
		t.Errorf("the 503 does not say what is missing: %s", body)
	}
}

// A discovery search matching nothing is an empty result, not an error: the
// provider answered, and its answer is "no such series".
func TestDiscoverMatchingNothingIsEmpty(t *testing.T) {
	reg := providers.New(nil)
	if err := reg.Register(
		providers.NewFake("fake-tvdb", providers.CapabilityMetadata)); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, withProviders(reg)).seed()

	resp := discover(h, `{"query":"nothing matches this"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("discover = %d: %s", resp.StatusCode, h.body(resp))
	}
	var out struct {
		Results []discoveryResult `json:"results"`
	}
	if err := json.Unmarshal(h.body(resp), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 0 {
		t.Errorf("results = %+v, want empty", out.Results)
	}
}

// Two providers offering the same series yield ONE result: discovery merges and
// deduplicates on (type, external id) rather than showing the same series twice.
func TestDiscoverDeduplicatesAcrossProviders(t *testing.T) {
	same := providers.DiscoveryCandidate{
		Title: "The Expanse", Year: 2015, ExternalID: "280619",
		Type: followed.TypeTVSeries,
	}
	reg := providers.New(nil)
	for _, name := range []string{"tvdb-a", "tvdb-b"} {
		f := providers.NewFake(name, providers.CapabilityMetadata).
			OfferDiscovery("the expanse", same)
		if err := reg.Register(f); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, withProviders(reg)).seed()

	resp := discover(h, `{"query":"the expanse"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("discover = %d: %s", resp.StatusCode, h.body(resp))
	}
	var out struct {
		Results []discoveryResult `json:"results"`
	}
	if err := json.Unmarshal(h.body(resp), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 {
		t.Errorf("the same series from two providers gave %d results, want 1", len(out.Results))
	}
}
