package podcast

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The podcast feed adapter, driven against a fixture RSS document over httptest —
// the only test an external feed will ever have (ADR-0026). The feed URL is the
// followed source's own FeedRef, so a test points the adapter at a fixture server
// simply by passing that server's URL as the ref; no endpoint is configured.

const feedXML = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd">
  <channel>
    <title>The Example Cast</title>
    <item>
      <title>Episode One</title>
      <guid isPermaLink="false">ep-0001</guid>
      <pubDate>Tue, 01 Sep 2026 08:00:00 +0000</pubDate>
      <itunes:duration>1800</itunes:duration>
      <enclosure url="https://cdn.example.com/audio/ep1.mp3" length="12345678" type="audio/mpeg"/>
    </item>
    <item>
      <title>Episode Two</title>
      <guid isPermaLink="false">ep-0002</guid>
      <pubDate>Tue, 08 Sep 2026 08:00:00 +0000</pubDate>
      <enclosure url="https://cdn.example.com/audio/ep2.mp3" length="22345678" type="audio/mpeg"/>
    </item>
    <item>
      <title>A note with no audio</title>
      <guid isPermaLink="false">ep-note</guid>
      <pubDate>Wed, 09 Sep 2026 08:00:00 +0000</pubDate>
    </item>
    <item>
      <title>Episode with no guid</title>
      <pubDate>Tue, 15 Sep 2026 08:00:00 +0000</pubDate>
      <enclosure url="https://cdn.example.com/audio/ep-noguid.mp3" length="9999" type="audio/mpeg"/>
    </item>
  </channel>
</rss>`

func fixtureServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{Name: "example-cast"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// THE assertion: a podcast RSS feed becomes one FeedItem per <item> that has an
// enclosure, keyed by guid, carrying the enclosure URL the poll projects as a
// direct release.
func TestEnumerateMapsItemsToFeedItems(t *testing.T) {
	srv := fixtureServer(t, http.StatusOK, feedXML)
	items, err := newClient(t).Enumerate(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	// Three of the four <item>s survive: the note with no enclosure is skipped
	// (nothing to fetch), the other three each carry an enclosure.
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3 (the enclosure-less note is skipped): %+v", len(items), items)
	}

	first := items[0]
	if first.Key != "ep-0001" {
		t.Errorf("first key = %q, want the guid ep-0001", first.Key)
	}
	if first.Title != "Episode One" {
		t.Errorf("first title = %q", first.Title)
	}
	if got := first.EnclosureURL(); got != "https://cdn.example.com/audio/ep1.mp3" {
		t.Errorf("first enclosure = %q", got)
	}
	if first.Attributes[followed.AttrEnclosureURL] == "" {
		t.Error("the enclosure URL must be under AttrEnclosureURL so the poll finds it")
	}
	if first.Attributes["enclosure_type"] != "audio/mpeg" || first.Attributes["enclosure_length"] != "12345678" {
		t.Errorf("enclosure metadata not carried: %+v", first.Attributes)
	}
	want := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	if !first.PublishedAt.Equal(want) {
		t.Errorf("first published = %v, want %v", first.PublishedAt, want)
	}

	// An item with no guid falls back to its enclosure URL as the stable key, so
	// it can still be deduped across polls.
	last := items[2]
	if last.Key != "https://cdn.example.com/audio/ep-noguid.mp3" {
		t.Errorf("a guid-less item should key on its enclosure URL, got %q", last.Key)
	}
}

// Enumerate is values-in, values-out and carries no transport: it must not be
// possible to tell a live feed from a fixture, so its capability and health are
// what routing reads.
func TestCapabilitiesAndHealth(t *testing.T) {
	c := newClient(t)
	caps := c.Capabilities()
	if len(caps) != 1 || caps[0] != providers.CapabilityMetadata {
		t.Errorf("capabilities = %v, want [metadata]", caps)
	}
	h := c.Check(t.Context())
	if !h.Healthy {
		t.Errorf("a podcast adapter has no central service to be unwell; health = %+v", h)
	}
	if h.CheckedAt.IsZero() {
		t.Error("Check must stamp when it looked")
	}
}

// A failed fetch is a call failure the poll loop must see and retry — folding it
// into an empty slice would report a source as having emitted nothing.
func TestEnumerateSurfacesAnHTTPError(t *testing.T) {
	srv := fixtureServer(t, http.StatusInternalServerError, "boom")
	if _, err := newClient(t).Enumerate(t.Context(), srv.URL); err == nil {
		t.Fatal("a 500 from the feed must be an error, not an empty enumeration")
	}
}

// An empty ref is refused rather than fetched, the same shape TVDB refuses an
// empty series id.
func TestEnumerateRefusesAnEmptyRef(t *testing.T) {
	if _, err := newClient(t).Enumerate(t.Context(), "  "); err == nil {
		t.Fatal("an empty feed URL must be refused")
	}
}

// The adapter satisfies the interfaces the registry routes on.
var (
	_ providers.Provider     = (*Client)(nil)
	_ providers.FeedProvider = (*Client)(nil)
)
