package indexers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Every test here drives the REAL prowlarrClient through httptest against
// captured-shape fixtures (ADR-0026: values in, values out, never a live
// Prowlarr). The fixtures are synthesised — there is no public Prowlarr to
// capture from — and shaped from Prowlarr's documented /api/v1 responses.

func prowlarrFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "prowlarr", name))
	if err != nil {
		t.Fatalf("reading fixture %q: %v", name, err)
	}
	return b
}

// prowlarrHarness serves the two routes the client uses, records the last search
// request for assertion, and enforces the api key when one is given.
type prowlarrHarness struct {
	srv       *httptest.Server
	lastQuery string
	lastKey   string
}

func newProwlarrHarness(t *testing.T, searchFixture, indexerFixture, requireKey string, searchStatus int) *prowlarrHarness {
	t.Helper()
	h := &prowlarrHarness{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		h.lastKey = r.Header.Get("X-Api-Key")
		h.lastQuery = r.URL.RawQuery
		if requireKey != "" && h.lastKey != requireKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if searchStatus != 0 {
			w.WriteHeader(searchStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(prowlarrFixture(t, searchFixture))
	})
	mux.HandleFunc("/api/v1/indexer", func(w http.ResponseWriter, r *http.Request) {
		if requireKey != "" && r.Header.Get("X-Api-Key") != requireKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if indexerFixture == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(prowlarrFixture(t, indexerFixture))
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func prowlarrClientFor(t *testing.T, endpoint, key string) *prowlarrClient {
	t.Helper()
	c, err := NewProwlarr(ProwlarrOptions{Name: "prowlarr", Endpoint: endpoint, APIKey: key})
	if err != nil {
		t.Fatalf("building prowlarr client: %v", err)
	}
	return c
}

// ADR-0093 §1: an episode-scoped want asks Prowlarr for "<Title> SxxEyy" rather
// than the bare series title, so the aggregate search returns the episode rather
// than every season's packs.
func TestProwlarrEpisodeSearchQueriesTheEpisode(t *testing.T) {
	h := newProwlarrHarness(t, "search-with-results.json", "indexers-two-enabled.json", "", 0)
	_, err := prowlarrClientFor(t, h.srv.URL, "").Search(t.Context(),
		providers.Query{Title: "Slow Horses", ContentType: "series", Season: 1, Episode: 1})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if !strings.Contains(h.lastQuery, "query=Slow+Horses+S01E01") {
		t.Errorf("episode search should query the episode, query was %q", h.lastQuery)
	}
}

// A real aggregate search answer becomes candidates: named releases only, with a
// stable id, credited to the provider, size carried, a fetchable source.
func TestProwlarrSearchBecomesCandidates(t *testing.T) {
	h := newProwlarrHarness(t, "search-with-results.json", "indexers-two-enabled.json", "", 0)
	got, err := prowlarrClientFor(t, h.srv.URL, "").Search(t.Context(), providers.Query{Title: "ubuntu"})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	// Three releases in the fixture, one nameless — the nameless one is dropped.
	if len(got) != 2 {
		t.Fatalf("want 2 candidates (nameless dropped), got %d", len(got))
	}
	for _, c := range got {
		if strings.TrimSpace(c.Title) == "" {
			t.Error("a candidate has no title")
		}
		if strings.TrimSpace(c.ID) == "" {
			t.Errorf("%q has no id", c.Title)
		}
		if c.Provider != "prowlarr" {
			t.Errorf("%q credits %q rather than the aggregating provider", c.Title, c.Provider)
		}
		if c.Source.Reveal() == "" {
			t.Errorf("%q has no fetchable source", c.Title)
		}
	}

	// The Ubuntu release carries an infohash (used directly, it is not a
	// credential), a magnet source (preferred over the download URL), and its
	// asserted size.
	var ubuntu bool
	for _, c := range got {
		if strings.HasPrefix(c.ID, "infohash:") {
			ubuntu = true
			if !strings.HasPrefix(c.Source.Reveal(), "magnet:") {
				t.Errorf("magnet should win over the download URL, got %q", c.Source.Reveal())
			}
			v, ok := c.Attributes[policy.AttrSizeBytes]
			if !ok {
				t.Error("size was asserted in the fixture but not carried")
			} else if v.Num != 6231920640 {
				t.Errorf("size = %d, want 6231920640", v.Num)
			}
		}
	}
	if !ubuntu {
		t.Error("no candidate got an infohash id from the fixture")
	}
}

// A private-tracker passkey in a download URL must not reach the candidate id.
func TestProwlarrCredentialDoesNotReachTheID(t *testing.T) {
	h := newProwlarrHarness(t, "search-with-results.json", "", "", 0)
	got, err := prowlarrClientFor(t, h.srv.URL, "").Search(t.Context(), providers.Query{Title: "ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if strings.Contains(c.ID, "SECRETKEY") || strings.Contains(c.ID, "passkey") {
			t.Errorf("candidate id %q leaked a credential", c.ID)
		}
	}
}

// An empty aggregate answer is a successful search that found nothing.
func TestProwlarrEmptySearchIsSuccessful(t *testing.T) {
	h := newProwlarrHarness(t, "search-empty.json", "", "", 0)
	got, err := prowlarrClientFor(t, h.srv.URL, "").Search(t.Context(), providers.Query{Title: "no-such-release"})
	if err != nil {
		t.Fatalf("an empty answer was reported as an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 candidates, got %d", len(got))
	}
}

// The search carries the api key on the header and the mapped category, never a
// key in the query string.
func TestProwlarrSearchSendsKeyHeaderAndCategory(t *testing.T) {
	h := newProwlarrHarness(t, "search-empty.json", "", "topsecret", 0)
	_, err := prowlarrClientFor(t, h.srv.URL, "topsecret").Search(t.Context(),
		providers.Query{Title: "dune", ContentType: "movie"})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if h.lastKey != "topsecret" {
		t.Errorf("api key was not sent on the X-Api-Key header, got %q", h.lastKey)
	}
	if strings.Contains(h.lastQuery, "topsecret") {
		t.Errorf("api key leaked into the query string: %q", h.lastQuery)
	}
	if !strings.Contains(h.lastQuery, "categories=2000") {
		t.Errorf("a movie search should map to category 2000, query was %q", h.lastQuery)
	}
	if !strings.Contains(h.lastQuery, "type=search") {
		t.Errorf("search type missing, query was %q", h.lastQuery)
	}
}

// Check reports reachability AND the enabled-indexer count.
func TestProwlarrCheckReportsIndexerCount(t *testing.T) {
	h := newProwlarrHarness(t, "search-empty.json", "indexers-two-enabled.json", "", 0)
	got := prowlarrClientFor(t, h.srv.URL, "").Check(t.Context())
	if !got.Healthy {
		t.Fatalf("a reachable Prowlarr with indexers should be healthy: %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "2 indexers") {
		t.Errorf("detail should count the two enabled indexers, got %q", got.Detail)
	}
}

// A reachable Prowlarr with nothing enabled is healthy-but-empty — a distinct,
// actionable state from unreachable.
func TestProwlarrCheckZeroIndexersIsHealthyButEmpty(t *testing.T) {
	h := newProwlarrHarness(t, "search-empty.json", "indexers-none-enabled.json", "", 0)
	got := prowlarrClientFor(t, h.srv.URL, "").Check(t.Context())
	if !got.Healthy {
		t.Errorf("reachable-but-empty should still be healthy, got %q", got.Detail)
	}
	if !strings.Contains(got.Detail, "no indexers are enabled") {
		t.Errorf("detail should name the empty state, got %q", got.Detail)
	}
}

// A rejected key is named as configuration, not smoothed into "unreachable".
func TestProwlarrRejectedKeyIsConfiguration(t *testing.T) {
	h := newProwlarrHarness(t, "search-empty.json", "indexers-two-enabled.json", "the-right-key", 0)
	c := prowlarrClientFor(t, h.srv.URL, "the-wrong-key")

	got := c.Check(t.Context())
	if got.Healthy {
		t.Error("a rejected key must not report healthy")
	}
	if !strings.Contains(got.Detail, "API key was rejected") {
		t.Errorf("detail should name the credential problem, got %q", got.Detail)
	}
	if _, err := c.Search(t.Context(), providers.Query{Title: "x"}); err == nil {
		t.Error("a search with a rejected key should error, not return an empty result")
	}
}

// A server error is reported unhealthy, never a panic.
func TestProwlarrServerErrorIsUnhealthy(t *testing.T) {
	h := newProwlarrHarness(t, "search-empty.json", "", "", http.StatusBadGateway) // "" indexer fixture → 500 on Check
	got := prowlarrClientFor(t, h.srv.URL, "").Check(t.Context())
	if got.Healthy {
		t.Errorf("an erroring Prowlarr should be unhealthy, got %q", got.Detail)
	}
}
