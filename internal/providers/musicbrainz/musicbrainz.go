// Package musicbrainz is the first CapabilityEnrich adapter for music (ADR-0087,
// M12 Phase 6): given a held music Work — the artist and album ingest parsed —
// it asks the MusicBrainz WS/2 API what the release canonically IS (its MBID, its
// real title and artist) and, via the linked Cover Art Archive, where its cover
// is, so the enrich worker can write the Work's external id, attach its cover,
// and — when the match is confident — correct the display identity (ADR-0088).
//
// # Keyless, but User-Agent is mandatory and the rate ceiling is real
//
// MusicBrainz is a public, keyless service (ADR-0087): no credential, no
// 1Password item, no sops secret. But it enforces two rules a well-behaved client
// must honour or be blocked: a descriptive User-Agent on every request (a client
// that omits one is refused), and ~1 request per second. The User-Agent is
// supplied at construction; the spacing is the shared proactive rate limiter
// (internal/providers/ratelimit) ADR-0087 predicted would gain its second user
// here.
//
// # The cover comes from the Cover Art Archive, keyed on the MBID
//
// MusicBrainz holds the identity; the Cover Art Archive holds the image, keyed on
// the release MBID at coverartarchive.org/release/{mbid}/front. This adapter does
// not fetch the image — it returns the URL, which the enrich worker fetches with
// the shared KindHTTP download client (the archive answers /front with a redirect
// to the image, which that client follows). So the Cover Art Archive is a URL
// this package constructs, not a second provider kind.
//
// # It is never exercised in CI (ADR-0026)
//
// MusicBrainz is an external service; per ADR-0026 the real client is driven only
// against a recorded corpus over httptest, and this package's tests do that.
package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/providers/enrichmatch"
	"github.com/rarebit-one/heyarr-core/internal/providers/ratelimit"
)

// defaultEndpoint is the MusicBrainz WS/2 base URL. Overridden by
// Options.Endpoint, which is how the fixture harness points the real client at an
// httptest server.
const defaultEndpoint = "https://musicbrainz.org/ws/2"

// coverArtBase is the Cover Art Archive release cover endpoint. /front redirects
// to the image; the worker's HTTP client follows it.
const coverArtBase = "https://coverartarchive.org/release"

// defaultTimeout bounds one call.
const defaultTimeout = 30 * time.Second

// maxBodyBytes bounds what will be read — a search page is a few KB of JSON.
const maxBodyBytes = 8 << 20

// maxReleases is how many release matches to ask for. MusicBrainz returns them
// scored, best first; enrichment takes the top usable hit.
const maxReleases = 5

// defaultMinInterval is MusicBrainz's ~1 req/s rule, with a little margin.
const defaultMinInterval = 1100 * time.Millisecond

// Options configure a MusicBrainz client.
type Options struct {
	// Name is the provider's name from configuration — the operator's word.
	Name string
	// Endpoint is the API base URL. Empty means defaultEndpoint. Tests set it to a
	// fixture server's URL.
	Endpoint string
	// UserAgent identifies heyarr to MusicBrainz, which REQUIRES a descriptive one
	// and blocks a client that omits it. Empty means a bare "heyarr".
	UserAgent string
	// HTTPClient is injected by tests. Nil means a client of this package's own
	// making.
	HTTPClient *http.Client
	// Now is the injected clock.
	Now func() time.Time
	// MinInterval overrides the request spacing. Zero means defaultMinInterval; a
	// negative value disables the limiter (tests not asserting spacing).
	MinInterval time.Duration
	// sleep is the limiter's sleep, injected only by tests. Nil means the
	// production context-aware sleep.
	sleep func(ctx context.Context, d time.Duration) error
}

// Client is one MusicBrainz enrich provider. It satisfies
// providers.EnrichProvider.
type Client struct {
	name      string
	endpoint  string
	userAgent string
	http      *http.Client
	now       func() time.Time
	limiter   *ratelimit.RateLimiter
}

// New builds a MusicBrainz client.
func New(o Options) (*Client, error) {
	if strings.TrimSpace(o.Name) == "" {
		return nil, errors.New("musicbrainz: a provider needs a name")
	}
	endpoint := strings.TrimRight(strings.TrimSpace(o.Endpoint), "/")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	httpClient := o.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	now := o.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	userAgent := strings.TrimSpace(o.UserAgent)
	if userAgent == "" {
		userAgent = "heyarr"
	}
	interval := o.MinInterval
	if interval == 0 {
		interval = defaultMinInterval
	}
	return &Client{
		name:      o.Name,
		endpoint:  endpoint,
		userAgent: userAgent,
		http:      httpClient,
		now:       now,
		limiter:   ratelimit.New(interval, now, o.sleep),
	}, nil
}

// Name is the operator's name for this provider.
func (c *Client) Name() string { return c.name }

// Capabilities is what this provider does: it is an enrich provider.
func (c *Client) Capabilities() []providers.Capability {
	return []providers.Capability{providers.CapabilityEnrich}
}

// ServesContentType reports that this adapter enriches music. Books are Open
// Library's; movies and series are not enriched (ADR-0087 §3).
func (c *Client) ServesContentType(ct string) bool {
	return strings.EqualFold(strings.TrimSpace(ct), "music")
}

// Check exercises the provider with a trivial release search and reports what it
// found. It EXERCISES rather than asserts (providers.Provider).
func (c *Client) Check(ctx context.Context) providers.Health {
	at := c.now().UTC()
	var body releaseSearchResponse
	if err := c.get(ctx, c.endpoint+"/release/?query=heyarr&fmt=json&limit=1", "search", &body); err != nil {
		return providers.Unhealthy(reachDetail(err), at)
	}
	return providers.Healthy("ws2", at)
}

// Enrich looks a held album up by artist+album and returns what MusicBrainz knows
// (providers.EnrichProvider). The bool is false with a nil error when nothing
// matched, which the worker backs off on rather than treating as an error.
func (c *Client) Enrich(ctx context.Context, q providers.EnrichQuery) (providers.EnrichResult, bool, error) {
	if !c.ServesContentType(q.ContentType) {
		return providers.EnrichResult{}, false, nil
	}
	album := strings.TrimSpace(q.Album)
	if album == "" {
		album = strings.TrimSpace(q.Title)
	}
	artist := strings.TrimSpace(q.Artist)
	if album == "" {
		return providers.EnrichResult{}, false, nil
	}

	path := fmt.Sprintf("%s/release/?query=%s&fmt=json&limit=%d",
		c.endpoint, neturl.QueryEscape(luceneQuery(artist, album)), maxReleases)
	var body releaseSearchResponse
	if err := c.get(ctx, path, "search", &body); err != nil {
		return providers.EnrichResult{}, false, err
	}

	for _, rel := range body.Releases {
		mbid := strings.TrimSpace(rel.ID)
		if mbid == "" {
			continue
		}
		relArtist := rel.artistName()
		res := providers.EnrichResult{
			ExternalIDs: map[string]string{"musicbrainz": mbid},
			CoverURL:    secret.Value(fmt.Sprintf("%s/%s/front", coverArtBase, mbid)),
			Title:       strings.TrimSpace(rel.Title),
			Author:      relArtist,
			Confidence:  confidence(artist, album, rel),
		}
		return res, true, nil
	}
	return providers.EnrichResult{}, false, nil
}

// luceneQuery builds the MusicBrainz search query. When an artist is known it is
// ANDed with the album for precision; with only an album (a Work whose artist
// ingest did not parse) the album alone is searched. Quotes group multi-word
// values; embedded quotes are dropped rather than escaped, since an album or
// artist name with a literal quote is vanishingly rare and a dropped quote still
// matches.
func luceneQuery(artist, album string) string {
	album = strings.ReplaceAll(album, `"`, "")
	if artist == "" {
		return fmt.Sprintf(`release:"%s"`, album)
	}
	artist = strings.ReplaceAll(artist, `"`, "")
	return fmt.Sprintf(`release:"%s" AND artist:"%s"`, album, artist)
}

// confidence scores a release against the query. It blends the token-set overlap
// (the scale the worker's threshold is calibrated on, shared with Open Library)
// with MusicBrainz's own 0..100 relevance score, taking the larger: a release
// MusicBrainz scored highly but whose words differ (a re-titled edition) is still
// trusted, and one it scored low but whose title+artist match exactly is too.
func confidence(artist, album string, rel release) float64 {
	tokens := enrichmatch.TokenSetRatio(album+" "+artist, rel.Title+" "+rel.artistName())
	mb := float64(rel.Score) / 100
	if mb > 1 {
		mb = 1
	}
	if mb > tokens {
		return mb
	}
	return tokens
}

// get performs a rate-limited GET and decodes the JSON body. MusicBrainz needs no
// credential; the mandatory User-Agent is set on every request.
func (c *Client) get(ctx context.Context, path, op string, into any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("musicbrainz: building %s request: %w", op, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("musicbrainz: %s request failed: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("musicbrainz: reading %s response: %w", op, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &httpError{status: resp.StatusCode, op: op}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("musicbrainz: decoding %s response: %w", op, err)
	}
	return nil
}

// httpError is a non-200 from MusicBrainz, carrying the status so Check can tell
// a throttle (503) or a bad request from an outage.
type httpError struct {
	status int
	op     string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("musicbrainz: %s returned HTTP %d", e.op, e.status)
}

// reachDetail turns a check error into a health detail. MusicBrainz needs no
// credential, so an error is reachability (or a 503 throttle), not auth.
func reachDetail(err error) string {
	var he *httpError
	if errors.As(err, &he) {
		if he.status == http.StatusServiceUnavailable {
			return "MusicBrainz is throttling (HTTP 503)"
		}
		return fmt.Sprintf("MusicBrainz returned HTTP %d", he.status)
	}
	return "could not reach MusicBrainz"
}

// releaseSearchResponse is the WS/2 /release search body. Only the fields an
// enrichment needs are read; the many others are tolerated.
type releaseSearchResponse struct {
	Releases []release `json:"releases"`
}

type release struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Score is MusicBrainz's own 0..100 relevance for this hit.
	Score        int            `json:"score"`
	ArtistCredit []artistCredit `json:"artist-credit"`
}

type artistCredit struct {
	Name   string `json:"name"`
	Artist struct {
		Name string `json:"name"`
	} `json:"artist"`
}

// artistName is the release's credited artist, joining a multi-artist credit's
// names in order. It prefers the credit's own name (which carries join phrases
// like "feat.") and falls back to the nested artist name.
func (r release) artistName() string {
	parts := make([]string, 0, len(r.ArtistCredit))
	for _, ac := range r.ArtistCredit {
		if n := strings.TrimSpace(ac.Name); n != "" {
			parts = append(parts, n)
			continue
		}
		if n := strings.TrimSpace(ac.Artist.Name); n != "" {
			parts = append(parts, n)
		}
	}
	return strings.Join(parts, " ")
}

var _ providers.EnrichProvider = (*Client)(nil)
