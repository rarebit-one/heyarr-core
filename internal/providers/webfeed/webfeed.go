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
// choose — so the adapter delegates parsing to webfeedparse, which sniffs the
// document's root element (<rss> vs <feed>) and maps both onto the same neutral
// Entry. The two wire shapes are matched by local element name, so the content:,
// dc: and media: namespaces a real feed also carries are simply ignored.
//
// # The source carries the feed URL alongside the article URL
//
// Each item's source is encoded as webcapture:<articleURL>\tfeed:<feedURL>.
// The tab separator is unambiguous — a tab is never valid in a URL (RFC 3986).
// The download client (downloads/webcapture) parses this form and uses the feed
// URL to fetch the feed's own full HTML content when the article page returns a
// blocking HTTP error (ADR-0110). Legacy sources without the feed segment are
// parsed as before: no tab means articleURL only, no fallback.
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/webfeedparse"
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

// ServesType reports that this adapter enumerates rss_feed sources.
func (c *Client) ServesType(t followed.Type) bool { return t == followed.TypeRSSFeed }

// Check reports the adapter ready. Like the podcast and youtube adapters there
// is no central service and no credential to exercise: a feed's address is the
// followed source's own FeedRef, checked per poll when Enumerate fetches it.
func (c *Client) Check(_ context.Context) providers.Health {
	return providers.Healthy("webfeed", c.now().UTC())
}

// Enumerate returns a publication's articles as neutral FeedItems. ref is the
// feed URL (RSS or Atom).
//
// It fetches the feed, delegates parsing to webfeedparse, and maps each entry to
// a FeedItem keyed by the entry's guid/id (falling back to its link), carrying
// webcapture:<articleURL>\tfeed:<feedURL> in AttrEnclosureURL so the poll
// projects it as a direct release the KindWebCapture client archives. The feed
// URL is embedded in the source so the download client can fall back to the
// feed's own article content when the article page is blocked (ADR-0110).
// An entry with no resolvable article URL is skipped rather than projected:
// an article heyarr cannot address is one it cannot capture.
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
	req.Header.Set("User-Agent", fmt.Sprintf("heyarr/%s ( +https://github.com/rarebit-one/heyarr-core )", buildinfo.Version))

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

	entries, err := webfeedparse.ParseFeed(raw)
	if err != nil {
		return nil, err
	}

	items := make([]followed.FeedItem, 0, len(entries))
	seen := make(map[string]bool)
	for _, e := range entries {
		article := strings.TrimSpace(e.Link)
		if article == "" {
			// No article URL — nothing to capture. Skipped, not projected.
			continue
		}
		// The guid/id is the source-stable identity that dedupes an article
		// across polls; an entry without one falls back to its link, which is
		// stable for the same reason a re-publish keeps the same URL.
		key := strings.TrimSpace(e.Key)
		if key == "" {
			key = article
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		// Embed the feed URL in the source so the download client can fall back
		// to the feed's own content when the article page is blocked (ADR-0110).
		source := followed.WebCaptureSourceScheme + article + "\tfeed:" + ref

		items = append(items, followed.FeedItem{
			Key:         key,
			Title:       strings.TrimSpace(e.Title),
			PublishedAt: e.Published,
			Attributes: map[string]string{
				followed.AttrEnclosureURL: source,
			},
		})
	}
	return items, nil
}
