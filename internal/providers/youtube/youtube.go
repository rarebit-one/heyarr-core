// Package youtube is a FeedProvider (M12 Phase 3): a metadata adapter that
// enumerates a YouTube channel's videos from its public channel RSS feed.
//
// # What makes it different from podcast, and what does not
//
// Like the podcast adapter it turns a public, key-less feed into neutral
// followed.FeedItems, and the poll loop projects each as a direct release with
// nothing to search for — the feed IS the discovery. What differs is the
// transport: a podcast <enclosure> is the audio file itself, fetchable by an
// ordinary HTTP GET, but a YouTube <link> is a watch PAGE, not the video bytes.
// So this adapter tags each item's source with followed.YtDlpSourceScheme, which
// routes it to the KindYtDlp download client (which runs yt-dlp) instead of the
// plain-HTTP one — see ADR-0062. Everything else — the FeedItem shape, the
// per-poll dedupe on Key, the from_now backfill — is identical.
//
// # The feed is Atom, not RSS
//
// YouTube serves youtube.com/feeds/videos.xml?channel_id=… as Atom: <feed> of
// <entry> elements, each carrying a yt:videoId, a title, a published timestamp
// and an alternate <link> to the watch URL. The wire shapes below are matched by
// local element name, so the yt: and media: namespaces a real feed also carries
// are simply ignored, exactly as the podcast adapter ignores itunes:.
//
// # It is never exercised in CI against a live feed
//
// A feed is an external service; per ADR-0026 the real adapter is driven only
// against recorded fixtures over httptest, which this package's tests do. There
// is no credential and no base endpoint: the feed URL is the followed source's
// own FeedRef, handed to Enumerate per poll, so a test points it at a fixture
// server by passing that server's URL as the ref.
package youtube

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// defaultTimeout bounds one feed fetch. A channel feed is a single, small Atom
// document; this is generous but bounded so a poll job holding a lease does not
// wait forever on a host that will never answer.
const defaultTimeout = 30 * time.Second

// maxBodyBytes bounds what will be read from a feed. YouTube's channel feed is
// the ~15 most recent videos — tens of KB — so this is a guard against a
// misdirected URL streaming forever, not a real limit on a feed.
const maxBodyBytes = 8 << 20

// Options configure a youtube client.
type Options struct {
	// Name is the provider's name from configuration — the operator's word,
	// used in health and routing.
	Name string
	// HTTPClient is injected by tests. Nil means a client of this package's own
	// making.
	HTTPClient *http.Client
	// Now is the injected clock.
	Now func() time.Time
}

// Client is one YouTube metadata provider. It satisfies providers.FeedProvider.
type Client struct {
	name string
	http *http.Client
	now  func() time.Time
}

// New builds a youtube client.
func New(o Options) (*Client, error) {
	if strings.TrimSpace(o.Name) == "" {
		return nil, errors.New("youtube: a provider needs a name")
	}
	httpClient := o.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	now := o.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Client{name: strings.TrimSpace(o.Name), http: httpClient, now: now}, nil
}

// Name is the operator's name for this provider.
func (c *Client) Name() string { return c.name }

// Capabilities is what this provider does: it is a metadata feed provider.
func (c *Client) Capabilities() []providers.Capability {
	return []providers.Capability{providers.CapabilityMetadata}
}

// ServesType reports that this adapter enumerates youtube_channel sources.
func (c *Client) ServesType(t followed.Type) bool { return t == followed.TypeYouTubeChannel }

// Check reports the adapter ready.
//
// As with the podcast adapter there is no central service and no credential to
// exercise: a channel feed's address is the followed source's own FeedRef, so
// what could be unreachable is a FEED, checked per poll when Enumerate fetches
// it, not a service this provider stands in front of. Reporting healthy here is
// honest — the parser is always ready — and a dead feed surfaces as an Enumerate
// error the poll job retries, the granularity that keeps one broken feed from
// holding off every other followed source.
func (c *Client) Check(_ context.Context) providers.Health {
	return providers.Healthy("youtube", c.now().UTC())
}

// Enumerate returns a channel's videos as neutral FeedItems. ref is the channel
// feed URL (youtube.com/feeds/videos.xml?channel_id=…).
//
// It fetches the feed and maps each <entry> to a FeedItem keyed by its video id,
// carrying followed.YtDlpSourceScheme+watchURL in AttrEnclosureURL so the poll
// projects it as a direct release the KindYtDlp client fetches. An entry with no
// resolvable video id or watch URL is skipped rather than projected: a video
// heyarr cannot address is one it cannot archive, and projecting it would strand
// a want with no release and no indexer to find one.
func (c *Client) Enumerate(ctx context.Context, ref string) ([]followed.FeedItem, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("youtube: a channel feed URL is required to enumerate videos")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return nil, fmt.Errorf("youtube: building the feed request: %w", err)
	}
	req.Header.Set("Accept", "application/atom+xml, application/xml;q=0.9, */*;q=0.8")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("youtube: fetching the feed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("youtube: reading the feed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("youtube: the feed returned HTTP %d", resp.StatusCode)
	}

	var feed atomFeed
	if err := xml.Unmarshal(raw, &feed); err != nil {
		return nil, fmt.Errorf("youtube: parsing the feed: %w", err)
	}

	items := make([]followed.FeedItem, 0, len(feed.Entries))
	seen := make(map[string]bool)
	for _, e := range feed.Entries {
		videoID := videoIDOf(e)
		if videoID == "" {
			// No stable id — nothing to dedupe or address. Skipped, not projected.
			continue
		}
		if seen[videoID] {
			continue
		}
		seen[videoID] = true

		watch := watchURLOf(e, videoID)
		attrs := map[string]string{
			followed.AttrEnclosureURL: followed.YtDlpSourceScheme + watch,
			"video_id":                videoID,
		}
		items = append(items, followed.FeedItem{
			Key:         videoID,
			Title:       strings.TrimSpace(e.Title),
			PublishedAt: parsePublished(e.Published),
			Attributes:  attrs,
		})
	}
	return items, nil
}

// videoIDOf resolves an entry's stable YouTube video id. The yt:videoId element
// is authoritative; the id element ("yt:video:VIDEOID") is the fallback for a
// feed that omitted it, and the watch link's v= query is the last resort. All
// three name the same id, so trying them in order tolerates a feed that is
// missing any one of them without inventing an identity.
func videoIDOf(e atomEntry) string {
	if v := strings.TrimSpace(e.VideoID); v != "" {
		return v
	}
	if id := strings.TrimSpace(e.ID); strings.HasPrefix(id, "yt:video:") {
		return strings.TrimPrefix(id, "yt:video:")
	}
	if href := alternateHref(e); href != "" {
		if u, err := url.Parse(href); err == nil {
			if v := strings.TrimSpace(u.Query().Get("v")); v != "" {
				return v
			}
		}
	}
	return ""
}

// watchURLOf is the canonical watch URL for a video. It is built from the video
// id rather than trusting the feed's link, so the URL handed to yt-dlp is always
// the plain, canonical form regardless of any tracking parameters or shortened
// host a feed's <link> might carry. The alternate link is used only as the
// source of the id when no id element was present (see videoIDOf).
func watchURLOf(_ atomEntry, videoID string) string {
	return "https://www.youtube.com/watch?v=" + url.QueryEscape(videoID)
}

// alternateHref returns the entry's alternate (human-facing) link href.
func alternateHref(e atomEntry) string {
	for _, l := range e.Links {
		if l.Rel == "" || l.Rel == "alternate" {
			if h := strings.TrimSpace(l.Href); h != "" {
				return h
			}
		}
	}
	return ""
}

// parsePublished parses an Atom <published> timestamp. Atom mandates RFC 3339, so
// that is tried first; a couple of near-variants are tolerated because feeds in
// the wild are not always strict. A missing or unparseable date is the zero
// time, which from_now backfill reads as "cannot show this is after the follow"
// (see the poll's shouldProject) and which is distinct from any real date.
func parsePublished(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z07:00",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// The wire shapes, kept unexported: nothing outside this package should couple
// to Atom's XML. Fields are matched by their local element name, so the yt: and
// media: namespaces a real channel feed also carries are simply ignored.
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID        string     `xml:"id"`
	VideoID   string     `xml:"videoId"`
	Title     string     `xml:"title"`
	Published string     `xml:"published"`
	Links     []atomLink `xml:"link"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
}
