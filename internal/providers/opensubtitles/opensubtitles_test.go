package opensubtitles

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/providers/fixtures"
)

// The replay suite (ADR-0026): OpenSubtitles is an external service reached with
// a credential and can never run in CI, so a recorded corpus driving the REAL
// client over httptest is the only test this adapter will ever have. Nothing
// here parses a fixture body directly — every corpus test drives the client's
// own transport, the only way to prove it builds the requests and reads the
// responses OpenSubtitles actually sends. The stateful paths (re-login on 401,
// the limiter, the cache) drive the same real client over a hand-rolled server
// or exercise the component directly, because the corpus server cannot sequence
// two different answers to one method+path.

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
	c, err := fixtures.Load(corpusRoot(t), "opensubtitles")
	if err != nil {
		t.Fatalf("loading opensubtitles corpus: %v", err)
	}
	return c
}

// newTestClient builds a client with the limiter and cache OFF by default, so a
// corpus test that makes two calls does not sleep and does not memoise: those two
// pieces of infrastructure are asserted on their own, where a fake clock proves
// the behaviour without a wall-clock wait.
func newTestClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := New(Options{
		Name:        "opensubtitles",
		Endpoint:    endpoint,
		APIKey:      "test-api-key",
		Username:    "user",
		Password:    "pass",
		UserAgent:   "heyarr-test",
		MinInterval: -1,
		CacheTTL:    -1,
		Now:         func() time.Time { return time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The static assertion the whole slice exists to make: an OpenSubtitles client
// IS a providers.SubtitleProvider, so the acquisition path routes to it by
// capability (ADR-0085).
func TestClientSatisfiesSubtitleProvider(t *testing.T) {
	var _ providers.SubtitleProvider = (*Client)(nil)
}

func TestCapabilityIsSubtitle(t *testing.T) {
	c := newTestClient(t, "http://example.invalid")
	caps := c.Capabilities()
	if len(caps) != 1 || caps[0] != providers.CapabilitySubtitle {
		t.Fatalf("capabilities = %v, want [subtitle]", caps)
	}
}

// Check exercises the authenticated /infos/languages endpoint; a 200 proves the
// api-key was accepted and reports healthy with the v1 version.
func TestCheckHealthy(t *testing.T) {
	srv := loadCorpus(t).Server()
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.http = srv.Client()

	h := c.Check(context.Background())
	if !h.Healthy {
		t.Fatalf("check = unhealthy (%s), want healthy", h.Detail)
	}
	if h.Version != "v1" {
		t.Errorf("version = %q, want v1", h.Version)
	}
}

// A rejected api-key answers 401, which Check reports as an auth failure — never
// an outage — so an operator fixes the key rather than chasing the network.
func TestCheckRejectedKey(t *testing.T) {
	ex, ok := loadCorpus(t).Find("unauthorised")
	if !ok {
		t.Fatal("the unauthorised fixture is missing")
	}
	srv := ex.ServeOne()
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.http = srv.Client()

	h := c.Check(context.Background())
	if h.Healthy {
		t.Fatal("check = healthy, want unhealthy for a 401")
	}
	if h.Detail != "the API key was rejected" {
		t.Errorf("detail = %q, want %q", h.Detail, "the API key was rejected")
	}
}

// An unreachable service is an outage, distinct from a rejected credential.
func TestCheckOutage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // closed: connecting fails, which is the outage shape.
	c := newTestClient(t, srv.URL)

	h := c.Check(context.Background())
	if h.Healthy {
		t.Fatal("check = healthy against a closed server")
	}
	if h.Detail != "could not reach OpenSubtitles" {
		t.Errorf("detail = %q, want %q", h.Detail, "could not reach OpenSubtitles")
	}
}

// The heart of the search: reading the two English subtitles for an episode and
// mapping each file to a neutral candidate carrying the id a resolve acts on and
// the ranking signals the adapter chooses by.
func TestFindSubtitle(t *testing.T) {
	srv := loadCorpus(t).Server()
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.http = srv.Client()

	got, err := c.FindSubtitle(context.Background(), providers.SubtitleQuery{
		IMDBID:    "0944947",
		Season:    1,
		Episode:   1,
		Languages: []string{"en"},
	})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}

	clean := got[0]
	if clean.FileID != "312" {
		t.Errorf("file id = %q, want 312", clean.FileID)
	}
	if clean.Language != "en" {
		t.Errorf("language = %q, want en", clean.Language)
	}
	if clean.HearingImpaired {
		t.Error("first candidate should not be hearing impaired")
	}
	if clean.DownloadCount != 1200 {
		t.Errorf("download count = %d, want 1200", clean.DownloadCount)
	}
	if clean.Format != "srt" {
		t.Errorf("format = %q, want srt", clean.Format)
	}
	if clean.Release != "Show.S01E01.1080p.WEB-DL" {
		t.Errorf("release = %q", clean.Release)
	}

	if !got[1].HearingImpaired {
		t.Error("second candidate should be hearing impaired")
	}
}

// An empty search is the modelled "nothing matched" outcome — no candidates and
// no error, so a caller does not read a fruitless search as a failed one.
func TestFindSubtitleEmpty(t *testing.T) {
	srv := loadCorpus(t).Server()
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.http = srv.Client()

	got, err := c.FindSubtitle(context.Background(), providers.SubtitleQuery{
		IMDBID:    "9999999",
		Languages: []string{"en"},
	})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d candidates, want 0", len(got))
	}
}

// ResolveSubtitle mints the JWT (lazily, via /login) and exchanges the file id
// for the temporary link and the quota counters. The link is a secret.Value, so
// the test reveals it deliberately to assert it.
func TestResolveSubtitle(t *testing.T) {
	srv := loadCorpus(t).Server()
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.http = srv.Client()

	link, err := c.ResolveSubtitle(context.Background(), "312")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if url := link.URL.Reveal(); url != "https://dl.opensubtitles.com/download/abc123/Show.S01E01.en.srt" {
		t.Errorf("url = %q", url)
	}
	if link.FileName != "Show.S01E01.en.srt" {
		t.Errorf("file name = %q", link.FileName)
	}
	if link.Remaining != 97 {
		t.Errorf("remaining = %d, want 97", link.Remaining)
	}
	if got := link.ResetsAt.Format(time.RFC3339); got != "2026-09-09T20:00:00Z" {
		t.Errorf("resets at = %s, want 2026-09-09T20:00:00Z", got)
	}
}

// A resolve whose first /download comes back 401 mints a fresh token ONCE and
// retries, because the ordinary reason a held token 401s is expiry. The server
// answers the first download 401 and the second 200, and logs in on demand.
func TestResolveReLoginsOnExpiredToken(t *testing.T) {
	var mu sync.Mutex
	var logins, downloads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/login":
			logins++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"token":"fresh-token","status":200}`))
		case "/download":
			downloads++
			if downloads == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"expired","status":401}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"link":"https://dl/x.srt","file_name":"x.srt","remaining":5,"reset_time_utc":"2026-09-09T20:00:00.000Z"}`))
		default:
			w.WriteHeader(599)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.http = srv.Client()

	link, err := c.ResolveSubtitle(context.Background(), "312")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if link.URL.Reveal() != "https://dl/x.srt" {
		t.Errorf("url = %q", link.URL.Reveal())
	}
	mu.Lock()
	defer mu.Unlock()
	if logins != 2 {
		t.Errorf("logins = %d, want 2 (one lazy, one after the 401)", logins)
	}
	if downloads != 2 {
		t.Errorf("downloads = %d, want 2 (the 401 and the retry)", downloads)
	}
}

// A second 401 after a fresh login is a rejected LOGIN, not an expired token, so
// it fails rather than looping — one re-login, then the error is returned.
func TestResolveGivesUpAfterSecondUnauthorized(t *testing.T) {
	var mu sync.Mutex
	var downloads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/login":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"token":"t","status":200}`))
		case "/download":
			downloads++
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"nope","status":401}`))
		default:
			w.WriteHeader(599)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	c.http = srv.Client()

	_, err := c.ResolveSubtitle(context.Background(), "312")
	if err == nil {
		t.Fatal("resolve succeeded, want an error after two 401s")
	}
	var he *httpError
	if !errors.As(err, &he) || he.status != http.StatusUnauthorized {
		t.Fatalf("error = %v, want an httpError with 401", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if downloads != 2 {
		t.Errorf("downloads = %d, want 2 (the first and the one retry)", downloads)
	}
}

// A non-numeric file id is refused before any request — the id comes from a
// candidate, and a candidate that cannot be resolved is a bug, not a fetch.
func TestResolveRejectsNonNumericFileID(t *testing.T) {
	c := newTestClient(t, "http://example.invalid")
	if _, err := c.ResolveSubtitle(context.Background(), "not-a-number"); err == nil {
		t.Fatal("resolve accepted a non-numeric file id")
	}
}

// The client memoises a search for a short window: a second identical query
// while the cache is fresh does not reach the service, which is what keeps a
// backfill's retries off the request budget.
func TestFindSubtitleUsesCache(t *testing.T) {
	var mu sync.Mutex
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"1","attributes":{"language":"en","format":"srt","files":[{"file_id":9,"file_name":"a.en.srt"}]}}]}`))
	}))
	defer srv.Close()
	c, err := New(Options{
		Name: "opensubtitles", Endpoint: srv.URL,
		APIKey: "k", Username: "u", Password: "p",
		MinInterval: -1, CacheTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.http = srv.Client()

	q := providers.SubtitleQuery{IMDBID: "1", Languages: []string{"en"}}
	for i := 0; i < 3; i++ {
		got, err := c.FindSubtitle(context.Background(), q)
		if err != nil {
			t.Fatalf("find %d: %v", i, err)
		}
		if len(got) != 1 || got[0].FileID != "9" {
			t.Fatalf("find %d: got %v", i, got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("service hit %d times, want 1 (the rest served from cache)", hits)
	}
}

// --- rate limiter ---

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// --- search cache ---

func TestSearchCacheHitAndExpiry(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)}
	c := newSearchCache(10*time.Minute, clk.now)
	cands := []providers.SubtitleCandidate{{FileID: "1", Language: "en"}}
	c.put("k", cands)

	got, ok := c.get("k")
	if !ok || len(got) != 1 || got[0].FileID != "1" {
		t.Fatalf("fresh get = %v, %v", got, ok)
	}

	clk.advance(11 * time.Minute)
	if _, ok := c.get("k"); ok {
		t.Error("an expired entry must miss")
	}
}

// The cache copies out, so a caller that sorts or trims its result cannot mutate
// what a later hit hands to someone else.
func TestSearchCacheCopiesOut(t *testing.T) {
	c := newSearchCache(time.Hour, func() time.Time { return time.Unix(0, 0) })
	c.put("k", []providers.SubtitleCandidate{{FileID: "1"}, {FileID: "2"}})

	got, _ := c.get("k")
	got[0].FileID = "mutated"

	again, _ := c.get("k")
	if again[0].FileID != "1" {
		t.Errorf("cache was mutated through a returned slice: %q", again[0].FileID)
	}
}

// A non-positive TTL disables the cache — every get misses.
func TestSearchCacheDisabled(t *testing.T) {
	c := newSearchCache(-1, nil)
	c.put("k", []providers.SubtitleCandidate{{FileID: "1"}})
	if _, ok := c.get("k"); ok {
		t.Error("a disabled cache must always miss")
	}
}
