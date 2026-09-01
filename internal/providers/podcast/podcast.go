// Package podcast is a FeedProvider (M12 Phase 2): a metadata adapter that
// enumerates a podcast's episodes from its RSS feed.
//
// # Why podcast-following is nearly free
//
// A podcast episode is audio — an ordinary media file — so it needs no new
// content model and no new transport. The RSS feed is BOTH the discovery and the
// bytes location: every <item> names its <enclosure>, a direct http(s) URL, so
// there is nothing to search for. This adapter turns the feed into neutral
// followed.FeedItems carrying that enclosure URL (followed.AttrEnclosureURL), and
// the poll loop projects each as a direct release the EXISTING KindHTTP download
// client fetches. The whole of Phase 2 is this parser plus that one attribute.
//
// # It is never exercised in CI against a live feed
//
// A feed is an external service; per ADR-0026 the real client is driven only
// against recorded fixtures over httptest, and this package's tests do exactly
// that. Unlike TVDB there is no credential and no base endpoint: the feed URL is
// the followed source's own FeedRef, handed to Enumerate per poll, so a test
// points it at a fixture server simply by passing that server's URL as the ref.
package podcast

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// defaultTimeout bounds one feed fetch. A feed is a single XML document; this is
// generous but bounded, so a poll job holding a lease does not wait forever on a
// host that will never answer.
const defaultTimeout = 30 * time.Second

// maxBodyBytes bounds what will be read from a feed. A large podcast back
// catalogue is a few MB of XML; this is a guard against a misdirected URL
// streaming forever, not a real limit on a feed.
const maxBodyBytes = 16 << 20

// Options configure a podcast client.
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

// Client is one podcast metadata provider. It satisfies providers.FeedProvider.
type Client struct {
	name string
	http *http.Client
	now  func() time.Time
}

// New builds a podcast client.
func New(o Options) (*Client, error) {
	if strings.TrimSpace(o.Name) == "" {
		return nil, errors.New("podcast: a provider needs a name")
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

// ServesType reports that this adapter enumerates podcast sources.
func (c *Client) ServesType(t followed.Type) bool { return t == followed.TypePodcast }

// Check reports the adapter ready.
//
// Unlike TVDB there is no central service and no credential to exercise: a
// podcast feed's address is the followed source's own FeedRef, so what could be
// unreachable is a FEED, checked per poll when Enumerate fetches it, not a
// service this provider stands in front of. Reporting healthy here is honest —
// the parser is always ready — and a dead feed surfaces as an Enumerate error
// the poll job retries, exactly the granularity that keeps one broken feed from
// holding off every other followed source.
func (c *Client) Check(_ context.Context) providers.Health {
	return providers.Healthy("rss", c.now().UTC())
}

// Enumerate returns a podcast's episodes as neutral FeedItems. ref is the feed
// URL.
//
// It fetches the feed and maps each <item> to a FeedItem keyed by its <guid>
// (falling back to the enclosure URL), carrying the enclosure URL in
// AttrEnclosureURL so the poll can project it as a direct release. An item with
// no enclosure is skipped rather than projected: a podcast entry heyarr cannot
// fetch is an entry it cannot archive, and projecting it would strand a want that
// has no release and no indexer to find one.
func (c *Client) Enumerate(ctx context.Context, ref string) ([]followed.FeedItem, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("podcast: a feed URL is required to enumerate episodes")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return nil, fmt.Errorf("podcast: building the feed request: %w", err)
	}
	req.Header.Set("Accept", "application/rss+xml, application/xml;q=0.9, */*;q=0.8")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("podcast: fetching the feed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("podcast: reading the feed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("podcast: the feed returned HTTP %d", resp.StatusCode)
	}

	var feed rss
	if err := xml.Unmarshal(raw, &feed); err != nil {
		return nil, fmt.Errorf("podcast: parsing the feed: %w", err)
	}

	items := make([]followed.FeedItem, 0, len(feed.Channel.Items))
	seen := make(map[string]bool)
	for _, it := range feed.Channel.Items {
		enclosure := strings.TrimSpace(it.Enclosure.URL)
		if enclosure == "" {
			// No bytes to fetch — nothing to archive. Skipped, not projected.
			continue
		}
		// The guid is the source-stable identity that dedupes an episode across
		// polls; an item without one falls back to the enclosure URL, which is
		// stable for the same reason a re-publish keeps the same file.
		key := strings.TrimSpace(it.GUID.Value)
		if key == "" {
			key = enclosure
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		attrs := map[string]string{followed.AttrEnclosureURL: enclosure}
		if t := strings.TrimSpace(it.Enclosure.Type); t != "" {
			attrs["enclosure_type"] = t
		}
		if l := strings.TrimSpace(it.Enclosure.Length); l != "" {
			attrs["enclosure_length"] = l
		}
		if g := strings.TrimSpace(it.GUID.Value); g != "" {
			attrs["guid"] = g
		}
		items = append(items, followed.FeedItem{
			Key:         key,
			Title:       strings.TrimSpace(it.Title),
			PublishedAt: parsePubDate(it.PubDate),
			Attributes:  attrs,
		})
	}
	return items, nil
}

// parsePubDate parses an RSS <pubDate>. RSS dates are RFC 822 with a four-digit
// year (RFC 1123); feeds vary between numeric and named zones, so a small set of
// layouts is tried. A missing or unparseable date is the zero time, which is
// distinct from any real date and which from_now backfill reads as "cannot show
// this is after the follow" (see the poll's shouldProject).
func parsePubDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC1123Z, // Mon, 02 Jan 2006 15:04:05 -0700
		time.RFC1123,  // Mon, 02 Jan 2006 15:04:05 MST
		time.RFC822Z,  // 02 Jan 06 15:04 -0700
		time.RFC822,   // 02 Jan 06 15:04 MST
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// The wire shapes, kept unexported: nothing outside this package should couple
// to RSS's XML. Fields are matched by their local element name, so the itunes:
// and content: namespaces a real feed also carries are simply ignored.
type rss struct {
	XMLName xml.Name `xml:"rss"`
	Channel channel  `xml:"channel"`
}

type channel struct {
	Items []item `xml:"item"`
}

type item struct {
	Title     string    `xml:"title"`
	GUID      guid      `xml:"guid"`
	PubDate   string    `xml:"pubDate"`
	Enclosure enclosure `xml:"enclosure"`
}

type guid struct {
	Value string `xml:",chardata"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Length string `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}
