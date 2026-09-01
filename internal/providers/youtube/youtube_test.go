package youtube

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The youtube feed adapter, driven against a fixture Atom document over httptest —
// the only test an external feed will ever have (ADR-0026). The channel feed URL
// is the followed source's own FeedRef, so a test points the adapter at a fixture
// server by passing that server's URL as the ref; no endpoint is configured.

// feedXML mirrors the shape YouTube serves at
// youtube.com/feeds/videos.xml?channel_id=…: an Atom <feed> of <entry>s in the
// yt: and media: namespaces. It deliberately exercises the id fallbacks: the
// second entry omits <yt:videoId> and must fall back to the <id>, and the third
// repeats the first entry's video to prove per-poll dedupe.
const feedXML = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns:yt="http://www.youtube.com/xml/schemas/2015"
      xmlns:media="http://search.yahoo.com/mrss/"
      xmlns="http://www.w3.org/2005/Atom">
  <title>Example Channel</title>
  <yt:channelId>UC_example_channel</yt:channelId>
  <entry>
    <id>yt:video:vid00000001</id>
    <yt:videoId>vid00000001</yt:videoId>
    <title>The First Video</title>
    <link rel="alternate" href="https://www.youtube.com/watch?v=vid00000001"/>
    <published>2026-09-01T08:00:00+00:00</published>
  </entry>
  <entry>
    <id>yt:video:vid00000002</id>
    <title>A Video Whose Feed Omitted yt:videoId</title>
    <link rel="alternate" href="https://www.youtube.com/watch?v=vid00000002"/>
    <published>2026-09-02T09:30:00+00:00</published>
  </entry>
  <entry>
    <id>yt:video:vid00000001</id>
    <yt:videoId>vid00000001</yt:videoId>
    <title>The First Video (a duplicate entry)</title>
    <link rel="alternate" href="https://www.youtube.com/watch?v=vid00000001"/>
    <published>2026-09-01T08:00:00+00:00</published>
  </entry>
</feed>`

func fixtureServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{Name: "example-channel"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// THE assertion: a channel Atom feed becomes one FeedItem per video, keyed by
// the video id, carrying a yt-dlp-tagged watch URL the poll projects as a direct
// release — and a duplicate entry does not become a second item.
func TestEnumerateMapsEntriesToFeedItems(t *testing.T) {
	srv := fixtureServer(t, http.StatusOK, feedXML)
	items, err := newClient(t).Enumerate(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	// Two of the three <entry>s survive: the third repeats the first video's id.
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (the duplicate is deduped): %+v", len(items), items)
	}

	first := items[0]
	if first.Key != "vid00000001" {
		t.Errorf("first key = %q, want the video id vid00000001", first.Key)
	}
	if first.Title != "The First Video" {
		t.Errorf("first title = %q", first.Title)
	}
	wantPub := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	if !first.PublishedAt.Equal(wantPub) {
		t.Errorf("first published = %v, want %v", first.PublishedAt, wantPub)
	}
}

// The enclosure carries the yt-dlp transport tag, not a bare watch URL, so the
// plain-HTTP download client refuses it and the KindYtDlp client claims it
// (ADR-0062). The watch URL is the canonical form built from the id.
func TestEnclosureIsYtDlpTagged(t *testing.T) {
	srv := fixtureServer(t, http.StatusOK, feedXML)
	items, err := newClient(t).Enumerate(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	got := items[0].EnclosureURL()
	want := followed.YtDlpSourceScheme + "https://www.youtube.com/watch?v=vid00000001"
	if got != want {
		t.Errorf("enclosure = %q, want %q", got, want)
	}
	// A non-empty enclosure is what tells the poll loop this is a direct release.
	if got == "" {
		t.Error("a youtube item must carry an enclosure so it is not sent to the search pipeline")
	}
}

// An entry that omits <yt:videoId> still resolves its id from the <id> element,
// so a feed missing one field does not silently drop a video.
func TestVideoIDFallsBackToIDElement(t *testing.T) {
	srv := fixtureServer(t, http.StatusOK, feedXML)
	items, err := newClient(t).Enumerate(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if items[1].Key != "vid00000002" {
		t.Errorf("second key = %q, want vid00000002 resolved from the <id> element", items[1].Key)
	}
}

// A feed the server could not serve is an Enumerate error the poll job retries,
// not an empty slice that would look like a channel with no videos.
func TestEnumerateSurfacesAnHTTPError(t *testing.T) {
	srv := fixtureServer(t, http.StatusInternalServerError, "nope")
	if _, err := newClient(t).Enumerate(t.Context(), srv.URL); err == nil {
		t.Fatal("a 500 must be an error, not an empty enumeration")
	}
}

// An empty ref is refused before any fetch: a source with no feed URL is a
// configuration mistake, not a channel that emitted nothing.
func TestEnumerateRefusesAnEmptyRef(t *testing.T) {
	if _, err := newClient(t).Enumerate(t.Context(), "   "); err == nil {
		t.Fatal("an empty feed URL must be refused")
	}
}

// The adapter declares exactly the metadata capability, so the registry routes a
// followed source's poll to it and never asks it to download or search.
func TestCapabilityIsMetadataOnly(t *testing.T) {
	caps := newClient(t).Capabilities()
	if len(caps) != 1 || caps[0] != providers.CapabilityMetadata {
		t.Errorf("capabilities = %v, want [metadata]", caps)
	}
}
