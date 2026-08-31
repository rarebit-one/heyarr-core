package tvdb

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/providers/fixtures"
)

// The replay suite (ADR-0026): TVDB is an external service with a credential and
// can never run in CI, so the recorded corpus driving the REAL client over
// httptest is the only test this adapter will ever have. Nothing here parses a
// fixture body directly — every test drives the client's own transport, which is
// the only way to prove it builds the requests and reads the responses TVDB
// actually sends.

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
	c, err := fixtures.Load(corpusRoot(t), "tvdb")
	if err != nil {
		t.Fatalf("loading tvdb corpus: %v", err)
	}
	return c
}

func newClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := New(Options{
		Name:     "thetvdb",
		Endpoint: endpoint,
		APIKey:   "a-test-key",
		Now:      func() time.Time { return time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The static assertion the whole slice exists to make: a TVDB client IS a
// providers.FeedProvider, so the follow beat routes to it by capability and a
// TMDB implementation later slots in behind the same interface.
func TestClientSatisfiesFeedProvider(t *testing.T) {
	var _ providers.FeedProvider = (*Client)(nil)
}

func TestCapabilityIsMetadata(t *testing.T) {
	c := newClient(t, "http://example.invalid")
	caps := c.Capabilities()
	if len(caps) != 1 || caps[0] != providers.CapabilityMetadata {
		t.Fatalf("capabilities = %v, want [metadata]", caps)
	}
}

// The heart of the adapter: logging in, walking the episodes, and mapping each
// to a neutral FeedItem keyed S..E.. with its air date and season/episode
// attributes — the calendar episode-following turns on.
func TestEnumerate(t *testing.T) {
	srv := loadCorpus(t).Server()
	defer srv.Close()
	c := newClient(t, srv.URL)
	c.http = srv.Client()

	items, err := c.Enumerate(context.Background(), "12345")
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}

	if items[0].Key != "S01E01" || items[1].Key != "S01E02" {
		t.Errorf("keys = %q, %q; want S01E01, S01E02", items[0].Key, items[1].Key)
	}
	if items[0].Title != "Pilot" || items[1].Title != "The Second One" {
		t.Errorf("titles = %q, %q", items[0].Title, items[1].Title)
	}
	if got := items[0].PublishedAt.Format("2006-01-02"); got != "2026-01-05" {
		t.Errorf("first air date = %s, want 2026-01-05", got)
	}
	if items[0].Attributes["season"] != "1" || items[0].Attributes["episode"] != "1" {
		t.Errorf("attributes = %v", items[0].Attributes)
	}
	if items[0].Attributes["tvdb_episode_id"] != "1001" {
		t.Errorf("episode id = %q, want 1001", items[0].Attributes["tvdb_episode_id"])
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
// would re-project on every poll.
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

// Check EXERCISES the provider: a good key logs in and reports healthy at v4.
func TestCheckHealthy(t *testing.T) {
	srv := loadCorpus(t).Server()
	defer srv.Close()
	c := newClient(t, srv.URL)
	c.http = srv.Client()

	h := c.Check(context.Background())
	if !h.Healthy {
		t.Fatalf("check = unhealthy (%s), want healthy", h.Detail)
	}
	if h.Version != "v4" {
		t.Errorf("version = %q, want v4", h.Version)
	}
	if !h.Checked() {
		t.Error("a check must record when it happened")
	}
}

// A rejected key is reported unhealthy without leaking the key — the detail
// names the cause so an operator can act, and 401 is told apart from an outage.
func TestCheckUnauthorised(t *testing.T) {
	ex, ok := loadCorpus(t).Find("login-unauthorised")
	if !ok {
		t.Fatal("the login-unauthorised fixture is missing")
	}
	srv := ex.ServeOne()
	defer srv.Close()
	c := newClient(t, srv.URL)
	c.http = srv.Client()

	h := c.Check(context.Background())
	if h.Healthy {
		t.Fatal("a rejected key must report unhealthy")
	}
	if h.Detail != "the API key was rejected" {
		t.Errorf("detail = %q, want it to name the rejected key", h.Detail)
	}
}
