package tmdb

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/providers/fixtures"
)

// The replay suite (ADR-0026): TMDB is an external service reached with a
// credential and can never run in CI, so the recorded corpus driving the REAL
// client over httptest is the only test this adapter will ever have. Nothing
// here parses a fixture body directly — every test drives the client's own
// transport, which is the only way to prove it builds the requests and reads the
// responses TMDB actually sends.

func corpusRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "fixtures", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func loadCorpus(t *testing.T) fixtures.Corpus {
	t.Helper()
	c, err := fixtures.Load(corpusRoot(t), "tmdb")
	if err != nil {
		t.Fatalf("loading tmdb corpus: %v", err)
	}
	return c
}

func newClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := New(Options{
		Name:     "themoviedb",
		Endpoint: endpoint,
		Token:    "a-test-token",
		Now:      func() time.Time { return time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The static assertion the whole slice exists to make: a TMDB client IS a
// providers.FeedProvider, so the follow beat routes to it by capability exactly
// as it does the TVDB adapter — the drop-in ADR-0058 promised.
func TestClientSatisfiesFeedProvider(t *testing.T) {
	var _ providers.FeedProvider = (*Client)(nil)
}

// A TMDB client is also a DiscoverySearcher (#451): the discovery door routes to
// it by that interface, beside TVDB.
func TestClientSatisfiesDiscoverySearcher(t *testing.T) {
	var _ providers.DiscoverySearcher = (*Client)(nil)
}

func TestCapabilityIsMetadata(t *testing.T) {
	c := newClient(t, "http://example.invalid")
	caps := c.Capabilities()
	if len(caps) != 1 || caps[0] != providers.CapabilityMetadata {
		t.Fatalf("capabilities = %v, want [metadata]", caps)
	}
}

// It serves tv_series and nothing else — movies are out of scope, and a podcast
// or channel is another adapter's source. This is what routes a poll to the
// RIGHT feed adapter when a deployment configures more than one.
func TestServesType(t *testing.T) {
	c := newClient(t, "http://example.invalid")
	if !c.ServesType(followed.TypeTVSeries) {
		t.Error("tmdb must serve tv_series")
	}
	for _, other := range []followed.Type{followed.TypePodcast, followed.TypeYouTubeChannel, followed.TypeRSSFeed} {
		if c.ServesType(other) {
			t.Errorf("tmdb must not serve %q", other)
		}
	}
}

// The heart of the adapter: reading the series' seasons, walking them in
// season-number order (the corpus lists them out of order to prove the sort),
// fetching each season's episodes, and mapping each to a neutral FeedItem keyed
// S..E.. with its air date and season/episode attributes — the calendar
// episode-following turns on. The unnumbered season-2 entry is skipped, so the
// three keyed episodes are what come back.
func TestEnumerate(t *testing.T) {
	srv := loadCorpus(t).Server()
	defer srv.Close()
	c := newClient(t, srv.URL)
	c.http = srv.Client()

	items, err := c.Enumerate(context.Background(), "12345")
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3 (the unnumbered episode is skipped)", len(items))
	}

	wantKeys := []string{"S01E01", "S01E02", "S02E01"}
	for i, want := range wantKeys {
		if items[i].Key != want {
			t.Errorf("key[%d] = %q, want %q", i, items[i].Key, want)
		}
	}
	if items[0].Title != "Pilot" || items[1].Title != "The Second One" || items[2].Title != "The Return" {
		t.Errorf("titles = %q, %q, %q", items[0].Title, items[1].Title, items[2].Title)
	}
	if got := items[0].PublishedAt.Format("2006-01-02"); got != "2026-01-05" {
		t.Errorf("first air date = %s, want 2026-01-05", got)
	}
	// The kept season-2 episode has an empty air_date — PublishedAt is the zero
	// time, distinct from any real date.
	if !items[2].PublishedAt.IsZero() {
		t.Errorf("S02E01 has an air date %v, want the zero time", items[2].PublishedAt)
	}
	if items[0].Attributes["season"] != "1" || items[0].Attributes["episode"] != "1" {
		t.Errorf("attributes = %v", items[0].Attributes)
	}
	if items[0].Attributes["tmdb_episode_id"] != "1001" {
		t.Errorf("episode id = %q, want 1001", items[0].Attributes["tmdb_episode_id"])
	}
	if items[2].Attributes["season"] != "2" || items[2].Attributes["episode"] != "1" {
		t.Errorf("S02E01 attributes = %v", items[2].Attributes)
	}

	// Every mapped item must be a FeedItem the domain accepts — one it would
	// refuse is not a projection, it is a bug.
	for _, it := range items {
		if err := it.Validate(); err != nil {
			t.Errorf("mapped item %q does not validate: %v", it.Key, err)
		}
	}
}

// Determinism, because these items drive per-item wants and an unstable order
// would re-project on every poll. The season sort is the load-bearing part.
func TestEnumerateIsDeterministic(t *testing.T) {
	srv := loadCorpus(t).Server()
	defer srv.Close()
	c := newClient(t, srv.URL)
	c.http = srv.Client()

	first, err := c.Enumerate(context.Background(), "12345")
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := c.Enumerate(context.Background(), "12345")
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(first) {
			t.Fatalf("length moved: %d then %d", len(first), len(again))
		}
		for i := range again {
			if again[i].Key != first[i].Key {
				t.Fatalf("order moved at %d: %q vs %q", i, first[i].Key, again[i].Key)
			}
		}
	}
}

// A ref is required — an empty series id cannot enumerate anything, and the
// refusal is local rather than an HTTP round trip.
func TestEnumerateRefusesEmptyRef(t *testing.T) {
	c := newClient(t, "http://example.invalid")
	if _, err := c.Enumerate(context.Background(), "  "); err == nil {
		t.Fatal("an empty series id must be refused")
	}
}

// Discover asks /search/tv for series matching the query and maps each hit to a
// neutral candidate carrying the TMDB id a follow acts on — skipping a hit with
// no id, which no follow could use.
func TestDiscover(t *testing.T) {
	srv := loadCorpus(t).Server()
	defer srv.Close()
	c := newClient(t, srv.URL)
	c.http = srv.Client()

	got, err := c.Discover(context.Background(), "test")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	// Three hits in the corpus, one with no id — so two candidates.
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2 (the id-less hit is skipped)", len(got))
	}
	if got[0].ExternalID != "100" || got[0].Title != "A Test Series" {
		t.Errorf("first candidate = %+v", got[0])
	}
	if got[0].Year != 2011 {
		t.Errorf("first candidate year = %d, want 2011", got[0].Year)
	}
	if got[0].Type != followed.TypeTVSeries {
		t.Errorf("candidate type = %q, want tv_series", got[0].Type)
	}
	if got[0].Overview == "" {
		t.Error("the first candidate lost its overview")
	}
	// The second hit has an empty overview and a first_air_date — both survive as
	// their honest values rather than being dropped.
	if got[1].ExternalID != "200" || got[1].Year != 2019 || got[1].Overview != "" {
		t.Errorf("second candidate = %+v", got[1])
	}
}

// A query is required — an empty query cannot search for anything, and the
// refusal is local rather than an HTTP round trip.
func TestDiscoverRefusesEmptyQuery(t *testing.T) {
	c := newClient(t, "http://example.invalid")
	if _, err := c.Discover(context.Background(), "  "); err == nil {
		t.Fatal("an empty query must be refused")
	}
}

// Check EXERCISES the provider: a good token fetches /configuration and reports
// healthy at v3.
func TestCheckHealthy(t *testing.T) {
	srv := loadCorpus(t).Server()
	defer srv.Close()
	c := newClient(t, srv.URL)
	c.http = srv.Client()

	h := c.Check(context.Background())
	if !h.Healthy {
		t.Fatalf("check = unhealthy (%s), want healthy", h.Detail)
	}
	if h.Version != "v3" {
		t.Errorf("version = %q, want v3", h.Version)
	}
	if !h.Checked() {
		t.Error("a check must record when it happened")
	}
}

// A rejected token is reported unhealthy without leaking the token — the detail
// names the cause so an operator can act, and 401 is told apart from an outage.
func TestCheckUnauthorised(t *testing.T) {
	ex, ok := loadCorpus(t).Find("configuration-unauthorised")
	if !ok {
		t.Fatal("the configuration-unauthorised fixture is missing")
	}
	srv := ex.ServeOne()
	defer srv.Close()
	c := newClient(t, srv.URL)
	c.http = srv.Client()

	h := c.Check(context.Background())
	if h.Healthy {
		t.Fatal("a rejected token must report unhealthy")
	}
	if h.Detail != "the API token was rejected" {
		t.Errorf("detail = %q, want it to name the rejected token", h.Detail)
	}
}
