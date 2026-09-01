// Package webfeed is a FeedProvider (M12 Phase 4): a metadata adapter that
// enumerates a web publication's articles from its RSS or Atom feed.
//
// # What makes web archiving different from podcast and youtube
//
// A podcast enclosure is an audio file and a YouTube link resolves to a video —
// both are media the existing content model already holds. An article is neither:
// its bytes are an HTML page, and the archive we want is that page made
// self-contained (ADR-0063). So like the youtube adapter this one tags each
// item's source for a capture client rather than a plain fetch — here
// followed.WebCaptureSourceScheme, which routes to KindWebCapture — and the
// captured single-file HTML lands as a managed Document asset. Everything else —
// the neutral FeedItem, the per-poll dedupe on Key, the from_now backfill — is
// the shape podcast and youtube already established.
//
// # It parses BOTH RSS and Atom
//
// A web feed is served as either, and a publication does not let a follower
// choose — so the adapter sniffs the document's root element (<rss> vs <feed>)
// and reads whichever it is, mapping both onto the same neutral FeedItem. The
// two wire shapes are matched by local element name, so the content:, dc: and
// media: namespaces a real feed also carries are simply ignored.
//
// # It is never exercised in CI against a live feed
//
// A feed is an external service; per ADR-0026 the real adapter is driven only
// against recorded fixtures over httptest, which this package's tests do. There
// is no credential and no base endpoint: the feed URL is the followed source's
// own FeedRef, handed to Enumerate per poll.
package webfeed

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

// defaultTimeout bounds one feed fetch. A feed is a single document; this is
// generous but bounded so a poll job holding a lease does not wait forever on a
// host that will never answer.
const defaultTimeout = 30 * time.Second

// maxBodyBytes bounds what will be read from a feed. A large article feed is a
// few MB of XML; this guards a misdirected URL streaming forever, not a real
// feed.
const maxBodyBytes = 16 << 20

// Options configure a webfeed client.
type Options struct {
	// Name is the provider's name from configuration — the operator's word, used
	// in health and routing.
	Name string
	// HTTPClient is injected by tests. Nil means a client of this package's own
	// making.
	HTTPClient *http.Client
	// Now is the injected clock.
	Now func() time.Time
}

// Client is one web-feed metadata provider. It satisfies providers.FeedProvider.
type Client struct {
	name string
	http *http.Client
	now  func() time.Time
}

// New builds a webfeed client.
func New(o Options) (*Client, error) {
	if strings.TrimSpace(o.Name) == "" {
		return nil, errors.New("webfeed: a provider needs a name")
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

// Check reports the adapter ready. Like the podcast and youtube adapters there
// is no central service and no credential to exercise: a feed's address is the
// followed source's own FeedRef, checked per poll when Enumerate fetches it.
func (c *Client) Check(_ context.Context) providers.Health {
	return providers.Healthy("webfeed", c.now().UTC())
}

// Enumerate returns a publication's articles as neutral FeedItems. ref is the
// feed URL (RSS or Atom).
//
// It fetches the feed, sniffs whether it is RSS or Atom, and maps each entry to
// a FeedItem keyed by the entry's guid/id (falling back to its link), carrying
// followed.WebCaptureSourceScheme+articleURL in AttrEnclosureURL so the poll
// projects it as a direct release the KindWebCapture client archives. An entry
// with no resolvable article URL is skipped rather than projected: an article
// heyarr cannot address is one it cannot capture.
func (c *Client) Enumerate(ctx context.Context, ref string) ([]followed.FeedItem, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("webfeed: a feed URL is required to enumerate articles")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return nil, fmt.Errorf("webfeed: building the feed request: %w", err)
	}
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, */*;q=0.8")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webfeed: fetching the feed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("webfeed: reading the feed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("webfeed: the feed returned HTTP %d", resp.StatusCode)
	}

	entries, err := parseFeed(raw)
	if err != nil {
		return nil, err
	}

	items := make([]followed.FeedItem, 0, len(entries))
	seen := make(map[string]bool)
	for _, e := range entries {
		article := strings.TrimSpace(e.link)
		if article == "" {
			// No article URL — nothing to capture. Skipped, not projected.
			continue
		}
		// The guid/id is the source-stable identity that dedupes an article
		// across polls; an entry without one falls back to its link, which is
		// stable for the same reason a re-publish keeps the same URL.
		key := strings.TrimSpace(e.key)
		if key == "" {
			key = article
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		items = append(items, followed.FeedItem{
			Key:         key,
			Title:       strings.TrimSpace(e.title),
			PublishedAt: e.published,
			Attributes: map[string]string{
				followed.AttrEnclosureURL: followed.WebCaptureSourceScheme + article,
			},
		})
	}
	return items, nil
}

// entry is the neutral shape both RSS and Atom are mapped onto before becoming
// FeedItems — the adapter's internal common denominator.
type entry struct {
	key       string
	title     string
	link      string
	published time.Time
}

// parseFeed sniffs the document's root element and reads it as RSS or Atom.
//
// Sniffing rather than trying both keeps a malformed feed an honest error: a
// document that is neither is reported as such, not silently read as an empty
// feed that would look like a publication with no articles.
func parseFeed(raw []byte) ([]entry, error) {
	root, err := rootElement(raw)
	if err != nil {
		return nil, err
	}
	switch root {
	case "rss", "rdf":
		var doc rssFeed
		if err := xml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("webfeed: parsing the RSS feed: %w", err)
		}
		channelItems := doc.channelItems()
		out := make([]entry, 0, len(channelItems))
		for _, it := range channelItems {
			out = append(out, entry{
				key:       firstNonEmpty(it.GUID.Value, it.Link),
				title:     it.Title,
				link:      it.Link,
				published: parseTime(it.PubDate, it.Date),
			})
		}
		return out, nil
	case "feed":
		var doc atomFeed
		if err := xml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("webfeed: parsing the Atom feed: %w", err)
		}
		out := make([]entry, 0, len(doc.Entries))
		for _, e := range doc.Entries {
			out = append(out, entry{
				key:       firstNonEmpty(e.ID, alternateHref(e)),
				title:     e.Title,
				link:      alternateHref(e),
				published: parseTime(e.Published, e.Updated),
			})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("webfeed: the document is neither RSS nor Atom (root <%s>)", root)
	}
}

// rootElement returns the local name of the document's first element.
func rootElement(raw []byte) (string, error) {
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("webfeed: the feed is not well-formed XML: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return strings.ToLower(se.Name.Local), nil
		}
	}
}

// alternateHref returns an Atom entry's alternate (human-facing) link, the
// article URL. It prefers rel="alternate" and falls back to a link with no rel,
// which Atom defines to mean alternate.
func alternateHref(e atomEntry) string {
	for _, l := range e.Links {
		if l.Rel == "alternate" {
			if h := strings.TrimSpace(l.Href); h != "" {
				return h
			}
		}
	}
	for _, l := range e.Links {
		if l.Rel == "" {
			if h := strings.TrimSpace(l.Href); h != "" {
				return h
			}
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// parseTime parses the first of the supplied timestamps that reads, trying RSS's
// RFC 822 layouts and Atom/Dublin-Core's RFC 3339. A missing or unparseable date
// is the zero time, which from_now backfill reads as "cannot show this is after
// the follow" (see the poll's shouldProject).
func parseTime(vals ...string) time.Time {
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		for _, layout := range []string{
			time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822,
			time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05Z07:00",
		} {
			if t, err := time.Parse(layout, v); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

// The wire shapes, kept unexported: nothing outside this package should couple
// to RSS/Atom XML. Fields are matched by local element name, so the namespaces a
// real feed also carries (content:, dc:, media:) are ignored — except dc:date,
// which is read as a fallback pubDate because RSS 1.0/RDF feeds carry only that.

type rssFeed struct {
	Channel rssChannel `xml:"channel"`
	// RSS 1.0 (RDF) puts <item> as a sibling of <channel>, not inside it; both
	// are accepted so an RDF feed is not read as empty.
	Items []rssItem `xml:"item"`
}

type rssChannel struct {
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title   string  `xml:"title"`
	Link    string  `xml:"link"`
	GUID    rssGUID `xml:"guid"`
	PubDate string  `xml:"pubDate"`
	Date    string  `xml:"date"` // dc:date, matched by local name
}

type rssGUID struct {
	Value string `xml:",chardata"`
}

type atomFeed struct {
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID        string     `xml:"id"`
	Title     string     `xml:"title"`
	Published string     `xml:"published"`
	Updated   string     `xml:"updated"`
	Links     []atomLink `xml:"link"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
}

// channelItems folds the RDF sibling-item case into the channel so parseFeed
// reads one place. It is a method rather than inline so the RSS branch stays a
// straight range over Channel.Items.
func (f rssFeed) channelItems() []rssItem {
	if len(f.Channel.Items) > 0 {
		return f.Channel.Items
	}
	return f.Items
}
