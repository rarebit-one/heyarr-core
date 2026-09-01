package webfeed

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The webfeed adapter, driven against fixture RSS and Atom documents over
// httptest — the only test an external feed will ever have (ADR-0026). The feed
// URL is the followed source's own FeedRef, so a test points the adapter at a
// fixture server by passing that server's URL as the ref; no endpoint is
// configured.

const rssXML = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/">
  <channel>
    <title>The Example Journal</title>
    <item>
      <title>First Article</title>
      <link>https://journal.example.com/first</link>
      <guid isPermaLink="false">post-0001</guid>
      <pubDate>Tue, 01 Sep 2026 08:00:00 +0000</pubDate>
    </item>
    <item>
      <title>Second Article</title>
      <link>https://journal.example.com/second</link>
      <guid isPermaLink="false">post-0002</guid>
      <pubDate>Wed, 02 Sep 2026 08:00:00 +0000</pubDate>
    </item>
    <item>
      <title>A duplicate of the first</title>
      <link>https://journal.example.com/first</link>
      <guid isPermaLink="false">post-0001</guid>
      <pubDate>Tue, 01 Sep 2026 08:00:00 +0000</pubDate>
    </item>
    <item>
      <title>An item with no link</title>
      <guid isPermaLink="false">post-nolink</guid>
    </item>
  </channel>
</rss>`

const atomXML = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>The Example Journal</title>
  <entry>
    <id>tag:journal.example.com,2026:first</id>
    <title>First Article</title>
    <link rel="alternate" href="https://journal.example.com/first"/>
    <published>2026-09-01T08:00:00Z</published>
  </entry>
  <entry>
    <id>tag:journal.example.com,2026:second</id>
    <title>Second Article</title>
    <link href="https://journal.example.com/second"/>
    <updated>2026-09-02T09:30:00Z</updated>
  </entry>
</feed>`

func fixtureServer(t *testing.T, status int, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{Name: "example-journal"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// An RSS feed becomes one FeedItem per <item> that has a link, keyed by guid,
// carrying a web-capture-tagged article URL — and a duplicate does not become a
// second item, an item with no link is skipped.
func TestEnumerateRSS(t *testing.T) {
	srv := fixtureServer(t, http.StatusOK, "application/rss+xml", rssXML)
	items, err := newClient(t).Enumerate(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (dup deduped, link-less skipped): %+v", len(items), items)
	}
	if items[0].Key != "post-0001" || items[0].Title != "First Article" {
		t.Errorf("first item = %+v", items[0])
	}
	wantPub := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	if !items[0].PublishedAt.Equal(wantPub) {
		t.Errorf("published = %v, want %v", items[0].PublishedAt, wantPub)
	}
	got := items[0].EnclosureURL()
	want := followed.WebCaptureSourceScheme + "https://journal.example.com/first"
	if got != want {
		t.Errorf("enclosure = %q, want %q", got, want)
	}
}

// An Atom feed maps onto the same FeedItems — including an entry whose alternate
// link carries no explicit rel (Atom defines a rel-less link as alternate).
func TestEnumerateAtom(t *testing.T) {
	srv := fixtureServer(t, http.StatusOK, "application/atom+xml", atomXML)
	items, err := newClient(t).Enumerate(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(items), items)
	}
	if items[1].EnclosureURL() != followed.WebCaptureSourceScheme+"https://journal.example.com/second" {
		t.Errorf("second enclosure = %q", items[1].EnclosureURL())
	}
	if items[1].Title != "Second Article" {
		t.Errorf("second title = %q", items[1].Title)
	}
}

// A document that is neither RSS nor Atom is an error, not an empty feed that
// would look like a publication with no articles.
func TestEnumerateRejectsNonFeed(t *testing.T) {
	srv := fixtureServer(t, http.StatusOK, "text/html", "<html><body>not a feed</body></html>")
	if _, err := newClient(t).Enumerate(t.Context(), srv.URL); err == nil {
		t.Fatal("a non-feed document must be an error")
	}
}

// A feed the server could not serve is an Enumerate error the poll job retries.
func TestEnumerateSurfacesHTTPError(t *testing.T) {
	srv := fixtureServer(t, http.StatusInternalServerError, "application/xml", "nope")
	if _, err := newClient(t).Enumerate(t.Context(), srv.URL); err == nil {
		t.Fatal("a 500 must be an error")
	}
}

// An empty ref is refused before any fetch.
func TestEnumerateRefusesEmptyRef(t *testing.T) {
	if _, err := newClient(t).Enumerate(t.Context(), "  "); err == nil {
		t.Fatal("an empty feed URL must be refused")
	}
}

// The adapter declares exactly the metadata capability.
func TestCapabilityIsMetadataOnly(t *testing.T) {
	caps := newClient(t).Capabilities()
	if len(caps) != 1 || caps[0] != providers.CapabilityMetadata {
		t.Errorf("capabilities = %v, want [metadata]", caps)
	}
}
