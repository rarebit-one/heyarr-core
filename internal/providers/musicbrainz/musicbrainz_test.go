package musicbrainz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// MusicBrainz is an external service; per ADR-0026 the real client is driven only
// against canned responses over httptest — values in, values out, no live
// service.

func newClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := New(Options{
		Name:        "musicbrainz",
		Endpoint:    endpoint,
		UserAgent:   "heyarr/test ( +https://example.invalid )",
		Now:         func() time.Time { return time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC) },
		MinInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientSatisfiesEnrichProvider(t *testing.T) {
	var _ providers.EnrichProvider = (*Client)(nil)
}

// A MusicBrainz client is also a DiscoverySearcher (#451, ADR-0077's deferred
// want-scoped half): the discovery door routes to it beside TVDB/TMDB.
func TestClientSatisfiesDiscoverySearcher(t *testing.T) {
	var _ providers.DiscoverySearcher = (*Client)(nil)
}

func TestCapabilityIsEnrich(t *testing.T) {
	caps := newClient(t, "http://example.invalid").Capabilities()
	if len(caps) != 1 || caps[0] != providers.CapabilityEnrich {
		t.Fatalf("capabilities = %v, want [enrich]", caps)
	}
}

func TestServesMusicOnly(t *testing.T) {
	c := newClient(t, "http://example.invalid")
	if !c.ServesContentType("music") || !c.ServesContentType("MUSIC") {
		t.Error("should serve music")
	}
	if c.ServesContentType("book") {
		t.Error("should serve only music")
	}
}

// A held album enriches to its release: the MBID comes back, the Cover Art
// Archive URL is minted from it, and the canonical title+artist come through
// with a high confidence.
func TestEnrichReturnsMBIDCoverAndCanonical(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"releases":[
			{"id":"abcd-1234","title":"Kind of Blue","score":100,"artist-credit":[{"name":"Miles Davis"}]}
		]}`))
	}))
	defer srv.Close()

	res, ok, err := newClient(t, srv.URL).Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "music",
		Title:       "Kind of Blue",
		Album:       "Kind of Blue",
		Artist:      "Miles Davis",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected a match")
	}
	if !strings.Contains(gotQuery, "Kind of Blue") || !strings.Contains(gotQuery, "Miles Davis") {
		t.Errorf("query = %q, want artist+release", gotQuery)
	}
	if res.ExternalIDs["musicbrainz"] != "abcd-1234" {
		t.Errorf("mbid = %q", res.ExternalIDs["musicbrainz"])
	}
	if res.CoverURL.Reveal() != "https://coverartarchive.org/release/abcd-1234/front" {
		t.Errorf("cover = %q", res.CoverURL.Reveal())
	}
	if res.Title != "Kind of Blue" || res.Author != "Miles Davis" {
		t.Errorf("canonical = %q / %q", res.Title, res.Author)
	}
	if res.Confidence < 0.9 {
		t.Errorf("confidence = %v, want high", res.Confidence)
	}
}

// With only an album (a Work whose artist ingest did not parse) the search is the
// album alone and still matches.
func TestEnrichAlbumOnly(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		_, _ = w.Write([]byte(`{"releases":[{"id":"e-9","title":"Blue","score":80,"artist-credit":[{"name":"Joni Mitchell"}]}]}`))
	}))
	defer srv.Close()

	_, ok, err := newClient(t, srv.URL).Enrich(context.Background(), providers.EnrichQuery{ContentType: "music", Title: "Blue"})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if strings.Contains(gotQuery, "artist:") {
		t.Errorf("query should not constrain artist: %q", gotQuery)
	}
}

func TestEnrichNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"releases":[]}`))
	}))
	defer srv.Close()

	_, ok, err := newClient(t, srv.URL).Enrich(context.Background(), providers.EnrichQuery{ContentType: "music", Title: "Nothing"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected no match")
	}
}

func TestEnrichWrongType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a book must not reach the music adapter")
	}))
	defer srv.Close()

	_, ok, err := newClient(t, srv.URL).Enrich(context.Background(), providers.EnrichQuery{ContentType: "book", Title: "A Book"})
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

// A multi-artist credit joins the names in order.
func TestArtistNameJoinsCredit(t *testing.T) {
	r := release{ArtistCredit: []artistCredit{{Name: "Brian Eno"}, {Name: "David Byrne"}}}
	if got := r.artistName(); got != "Brian Eno David Byrne" {
		t.Errorf("artistName = %q", got)
	}
}

// Discover runs the query as-is (no artist/album split the way Enrich's
// luceneQuery does) and maps each release with a usable MBID to a neutral
// "music" candidate a caller wants (there is no follow door for a release).
func TestDiscoverReturnsMusicCandidates(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"releases":[
			{"id":"a1b2c3d4-0000-0000-0000-000000000001","title":"My Life in the Bush of Ghosts","score":100,"date":"1981-02-01","artist-credit":[{"name":"Brian Eno"},{"name":"David Byrne"}]},
			{"id":"","title":"A Release With No MBID","score":50,"date":"1999"}
		]}`))
	}))
	defer srv.Close()

	got, err := newClient(t, srv.URL).Discover(context.Background(), "bush of ghosts")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	// Two releases in the fixture, one with no MBID — so one candidate.
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1 (the id-less release is skipped): %+v", len(got), got)
	}
	if gotQuery != "bush of ghosts" {
		t.Errorf("query = %q, want the exact free-text query", gotQuery)
	}
	c := got[0]
	if c.Title != "My Life in the Bush of Ghosts" || c.Year != 1981 {
		t.Errorf("candidate = %+v", c)
	}
	if c.ExternalID != "a1b2c3d4-0000-0000-0000-000000000001" {
		t.Errorf("external id = %q", c.ExternalID)
	}
	if c.Source != "musicbrainz" || c.Type != "music" {
		t.Errorf("source/type = %q/%q, want musicbrainz/music", c.Source, c.Type)
	}
	if c.Overview != "by Brian Eno David Byrne" {
		t.Errorf("overview = %q", c.Overview)
	}
}

// An empty query is refused locally — no HTTP round trip for nothing to search.
func TestDiscoverRefusesEmptyQuery(t *testing.T) {
	c := newClient(t, "http://example.invalid")
	if _, err := c.Discover(context.Background(), "  "); err == nil {
		t.Fatal("an empty query must be refused")
	}
}

// A query matching nothing is the modelled empty result, not an error.
func TestDiscoverMatchingNothingIsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"releases":[]}`))
	}))
	defer srv.Close()

	got, err := newClient(t, srv.URL).Discover(context.Background(), "nothing matches this")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d candidates, want 0", len(got))
	}
}

// releaseYear reads the leading YYYY off MusicBrainz's partial-precision dates,
// and a date too short or malformed to hold one is the honest "no year" zero.
func TestReleaseYear(t *testing.T) {
	cases := map[string]int{
		"1981-02-01": 1981,
		"1981-02":    1981,
		"1981":       1981,
		"":           0,
		"81":         0,
		"abcd-02-01": 0,
	}
	for date, want := range cases {
		if got := releaseYear(date); got != want {
			t.Errorf("releaseYear(%q) = %d, want %d", date, got, want)
		}
	}
}
