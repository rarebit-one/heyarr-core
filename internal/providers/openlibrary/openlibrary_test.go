package openlibrary

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Open Library is an external service; per ADR-0026 the real client is driven
// only against canned responses over httptest — values in, values out, no live
// service. Nothing here parses a fixture body directly; every test drives the
// client's own transport.

func newClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := New(Options{
		Name:        "openlibrary",
		Endpoint:    endpoint,
		Now:         func() time.Time { return time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC) },
		MinInterval: -1, // disable spacing; these tests do not assert it
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The static assertion the whole slice exists to make: an Open Library client IS
// a providers.EnrichProvider, so the enrich worker routes to it by capability.
func TestClientSatisfiesEnrichProvider(t *testing.T) {
	var _ providers.EnrichProvider = (*Client)(nil)
}

func TestCapabilityIsEnrich(t *testing.T) {
	c := newClient(t, "http://example.invalid")
	caps := c.Capabilities()
	if len(caps) != 1 || caps[0] != providers.CapabilityEnrich {
		t.Fatalf("capabilities = %v, want [enrich]", caps)
	}
}

func TestServesBooksOnly(t *testing.T) {
	c := newClient(t, "http://example.invalid")
	if !c.ServesContentType("book") || !c.ServesContentType("BOOK") {
		t.Error("should serve book")
	}
	if c.ServesContentType("music") || c.ServesContentType("movie") {
		t.Error("should serve only book")
	}
}

// A noisy shelf title enriches to the canonical work: the request carries the
// CLEANED query (provenance junk stripped), and the top hit's OLID, cover and
// canonical title+author come back with a high confidence.
func TestEnrichCleansTitleAndReturnsCanonical(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"docs":[
			{"key":"/works/OL123W","title":"The Almanack of Naval Ravikant","author_name":["Eric Jorgenson"],"cover_i":8901,"first_publish_year":2020}
		]}`))
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	res, ok, err := c.Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "book",
		Title:       "The Almanack Of Naval Ravikant Eric Jorgenson Z Library",
		Author:      "Books",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected a match")
	}
	// The junk tokens were stripped from the query.
	if strings.Contains(strings.ToLower(gotQuery), "library") || strings.Contains(strings.ToLower(gotQuery), " z ") {
		t.Errorf("query still carries junk: %q", gotQuery)
	}
	if res.ExternalIDs["openlibrary"] != "OL123W" {
		t.Errorf("olid = %q", res.ExternalIDs["openlibrary"])
	}
	if res.CoverURL.Reveal() != "https://covers.openlibrary.org/b/id/8901-L.jpg" {
		t.Errorf("cover = %q", res.CoverURL.Reveal())
	}
	if res.Title != "The Almanack of Naval Ravikant" || res.Author != "Eric Jorgenson" {
		t.Errorf("canonical = %q / %q", res.Title, res.Author)
	}
	// The cleaned query "the almanack of naval ravikant eric jorgenson" is fully
	// contained in the hit's title+author, so confidence is at the top.
	if res.Confidence < 0.9 {
		t.Errorf("confidence = %v, want high", res.Confidence)
	}
}

// A doc with no cover leaves CoverURL empty rather than minting a broken URL.
func TestEnrichNoCover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"docs":[{"key":"/works/OL9W","title":"Obscure Book","author_name":["A Writer"]}]}`))
	}))
	defer srv.Close()

	res, ok, err := newClient(t, srv.URL).Enrich(context.Background(), providers.EnrichQuery{ContentType: "book", Title: "Obscure Book"})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if res.CoverURL.Reveal() != "" {
		t.Errorf("cover = %q, want empty", res.CoverURL.Reveal())
	}
}

// An empty docs array is the modelled "nothing matched": false, no error.
func TestEnrichNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"docs":[]}`))
	}))
	defer srv.Close()

	_, ok, err := newClient(t, srv.URL).Enrich(context.Background(), providers.EnrichQuery{ContentType: "book", Title: "Nothing Here"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected no match")
	}
}

// A non-book content type is declined without a call.
func TestEnrichWrongType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("music must not reach the book adapter")
	}))
	defer srv.Close()

	_, ok, err := newClient(t, srv.URL).Enrich(context.Background(), providers.EnrichQuery{ContentType: "music", Title: "An Album"})
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestCleanTitleStripsNoise(t *testing.T) {
	got := cleanTitle("The Anxious Generation Jonathan Haidt Z Library")
	if strings.Contains(got, "library") || strings.Contains(got, "z ") {
		t.Errorf("cleanTitle left noise: %q", got)
	}
	if !strings.Contains(got, "anxious") || !strings.Contains(got, "haidt") {
		t.Errorf("cleanTitle dropped real words: %q", got)
	}
}

func TestWorkOLID(t *testing.T) {
	if got := workOLID("/works/OL42W"); got != "OL42W" {
		t.Errorf("workOLID = %q", got)
	}
	if got := workOLID("/books/OL7M"); got != "" {
		t.Errorf("non-work key should be empty, got %q", got)
	}
}
