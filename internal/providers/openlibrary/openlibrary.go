// Package openlibrary is the first CapabilityEnrich adapter for books (ADR-0087,
// M12 Phase 6): given a held book Work — the author and title ingest parsed — it
// asks Open Library's search API what the book canonically IS (its OLID, its
// cover, its real title and author) so the enrich worker can write the Work's
// external id, attach its cover, and — when the match is confident — correct the
// noisy display identity a filename produced (ADR-0088).
//
// # Keyless, and courteous
//
// Open Library is a public, keyless service (ADR-0087): there is no credential,
// no 1Password item and no sops secret — the whole credential story of the video
// and subtitle providers does not apply. It is a free service run by a nonprofit,
// so this client is a courteous one: it sends a descriptive User-Agent and
// spaces its requests through the shared proactive rate limiter
// (internal/providers/ratelimit), the same infrastructure OpenSubtitles built.
//
// # The library's book identity is noise, so the query is cleaned first
//
// The books this adapter enriches were identified from a shelf layout — the
// "author" is a shelf name ("Books"/"Reads"/"Review"), and the real author is
// mashed into the title with provenance junk ("Z Library", "Kepub") — see
// ADR-0088. So the query is the CLEANED title (junk tokens stripped) run as
// Open Library's free-text q= search, not a title+author filter that a shelf-name
// author would poison. The top hit is taken, and a confidence is computed as the
// token-set overlap (internal/providers/enrichmatch) between the cleaned query
// and the hit's title+author — high when the query's words are all present in
// the canonical record, low when the top hit barely resembles what was asked. The
// worker adopts the canonical title+author as the Work's display identity only
// when that confidence clears its threshold (ADR-0088 §3).
//
// # It is never exercised in CI (ADR-0026)
//
// Open Library is an external service; per ADR-0026 the real client is driven
// only against a recorded corpus over httptest, and this package's tests do
// exactly that.
package openlibrary

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

// defaultEndpoint is Open Library's base URL. Overridden by Options.Endpoint,
// which is how the fixture harness points the real client at an httptest server.
const defaultEndpoint = "https://openlibrary.org"

// coverBase is where a cover image is fetched by its cover id. It is a separate
// host from the API and needs no key; the "-L" suffix asks for the large size.
const coverBase = "https://covers.openlibrary.org/b/id"

// defaultTimeout bounds one call. A search is a small JSON round trip; generous
// but bounded, so a job holding a lease does not wait forever.
const defaultTimeout = 30 * time.Second

// maxBodyBytes bounds what will be read. A search page is a few KB of JSON with
// a handful of docs (limit=maxDocs); this is a guard against a misdirected
// endpoint, not a real limit.
const maxBodyBytes = 8 << 20

// maxDocs is how many search results to ask for. Open Library returns the most
// relevant first, and enrichment takes the top hit; a few extras cost nothing and
// leave room to skip a doc that carries no usable id.
const maxDocs = 5

// defaultMinInterval spaces requests as a courtesy to a free public service —
// well under a request per second.
const defaultMinInterval = time.Second

// Options configure an Open Library client.
type Options struct {
	// Name is the provider's name from configuration — the operator's word, used
	// in health and routing.
	Name string
	// Endpoint is the API base URL. Empty means defaultEndpoint. Tests set it to a
	// fixture server's URL.
	Endpoint string
	// UserAgent identifies heyarr to the service. Open Library asks callers to
	// send a descriptive one so it can attribute and, if need be, contact traffic.
	// Empty means a bare "heyarr".
	UserAgent string
	// HTTPClient is injected by tests. Nil means a client of this package's own
	// making.
	HTTPClient *http.Client
	// Now is the injected clock.
	Now func() time.Time
	// MinInterval overrides the request spacing. Zero means defaultMinInterval; a
	// negative value disables the limiter (tests not asserting spacing).
	MinInterval time.Duration
	// sleep is the limiter's sleep, injected only by tests so spacing is asserted
	// without a real wait. Nil means the production context-aware sleep.
	sleep func(ctx context.Context, d time.Duration) error
}

// Client is one Open Library enrich provider. It satisfies
// providers.EnrichProvider.
type Client struct {
	name      string
	endpoint  string
	userAgent string
	http      *http.Client
	now       func() time.Time
	limiter   *ratelimit.RateLimiter
}

// New builds an Open Library client.
func New(o Options) (*Client, error) {
	if strings.TrimSpace(o.Name) == "" {
		return nil, errors.New("openlibrary: a provider needs a name")
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

// ServesContentType reports that this adapter enriches books. Music is
// MusicBrainz's; movies and series are not enriched (ADR-0087 §3).
func (c *Client) ServesContentType(ct string) bool {
	return strings.EqualFold(strings.TrimSpace(ct), "book")
}

// Check exercises the provider by running a trivial search, and reports what it
// found. It EXERCISES rather than asserts (providers.Provider): a service that is
// unreachable must report unhealthy so enrichment does not route to it and stall.
func (c *Client) Check(ctx context.Context) providers.Health {
	at := c.now().UTC()
	var body searchResponse
	if err := c.get(ctx, c.endpoint+"/search.json?q=heyarr&limit=1", "search", &body); err != nil {
		return providers.Unhealthy(reachDetail(err), at)
	}
	return providers.Healthy("openlibrary", at)
}

// Enrich looks a held book up by its cleaned title and returns what Open Library
// knows (providers.EnrichProvider). The bool is false with a nil error when
// nothing matched — a book the catalogue does not know — which the worker backs
// off on rather than treating as an error.
func (c *Client) Enrich(ctx context.Context, q providers.EnrichQuery) (providers.EnrichResult, bool, error) {
	if !c.ServesContentType(q.ContentType) {
		return providers.EnrichResult{}, false, nil
	}
	cleaned := cleanTitle(q.Title)
	if cleaned == "" {
		// Nothing usable to search by is not an error — it is a Work whose
		// filename left no title to match, which backs off like a miss.
		return providers.EnrichResult{}, false, nil
	}

	path := fmt.Sprintf("%s/search.json?q=%s&limit=%d&fields=key,title,author_name,cover_i,cover_edition_key,first_publish_year",
		c.endpoint, neturl.QueryEscape(cleaned), maxDocs)
	var body searchResponse
	if err := c.get(ctx, path, "search", &body); err != nil {
		return providers.EnrichResult{}, false, err
	}

	for _, doc := range body.Docs {
		olid := workOLID(doc.Key)
		if olid == "" {
			// A doc with no work key cannot be recorded as an id — skip it rather
			// than surface an unrecordable match.
			continue
		}
		author := ""
		if len(doc.AuthorName) > 0 {
			author = strings.TrimSpace(doc.AuthorName[0])
		}
		res := providers.EnrichResult{
			ExternalIDs: map[string]string{"openlibrary": olid},
			Title:       strings.TrimSpace(doc.Title),
			Author:      author,
			Confidence:  enrichmatch.TokenSetRatio(cleaned, doc.Title+" "+strings.Join(doc.AuthorName, " ")),
		}
		if doc.CoverID > 0 {
			res.CoverURL = secret.Value(fmt.Sprintf("%s/%d-L.jpg", coverBase, doc.CoverID))
		}
		return res, true, nil
	}
	return providers.EnrichResult{}, false, nil
}

// get performs a rate-limited GET and decodes the JSON body. Open Library needs
// no credential, so nothing here carries a secret; op is stamped into a non-200
// error so a failed search or health check are told apart in a health detail.
func (c *Client) get(ctx context.Context, path, op string, into any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("openlibrary: building %s request: %w", op, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openlibrary: %s request failed: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("openlibrary: reading %s response: %w", op, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &httpError{status: resp.StatusCode, op: op}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("openlibrary: decoding %s response: %w", op, err)
	}
	return nil
}

// httpError is a non-200 from Open Library, carrying the status so Check can tell
// an outage from a bad request.
type httpError struct {
	status int
	op     string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("openlibrary: %s returned HTTP %d", e.op, e.status)
}

// reachDetail turns a check error into a health detail. Open Library needs no
// credential, so an error is reachability, not auth.
func reachDetail(err error) string {
	var he *httpError
	if errors.As(err, &he) {
		return fmt.Sprintf("Open Library returned HTTP %d", he.status)
	}
	return "could not reach Open Library"
}

// noiseTokens are the provenance and format words the shelf-scraped filenames
// carry that are not part of a book's title (ADR-0088). They are stripped from a
// query title before the search so the match is on the real words.
var noiseTokens = map[string]bool{
	"z": true, "zlibrary": true, "zlib": true, "library": true,
	"kepub": true, "epub": true, "mobi": true, "azw": true, "azw3": true,
	"pdf": true, "retail": true, "ebook": true, "annas": true, "archive": true,
	"calibre": true, "org": true,
}

// cleanTitle strips the provenance/format noise a shelf-scraped filename carries
// and returns the remaining words as a search query. It lower-cases, folds
// punctuation to spaces, drops the noise tokens and single characters, and
// rejoins — "The Almanack Of Naval Ravikant Eric Jorgenson Z Library" becomes
// "the almanack of naval ravikant eric jorgenson", which Open Library's
// relevance search resolves even with the author words still present.
func cleanTitle(title string) string {
	lower := strings.ToLower(title)
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		// Split on anything that is not a word rune, so punctuation and spaces
		// both separate. A letter or digit is part of a word; everything else is
		// a boundary.
		return !isWordRune(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if noiseTokens[f] {
			continue
		}
		if len([]rune(f)) < 2 {
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// isWordRune reports whether r is part of a word (a lower-case letter or a
// digit). The title is lower-cased before this is applied, so upper-case letters
// need not be admitted here.
func isWordRune(r rune) bool {
	return ('a' <= r && r <= 'z') || ('0' <= r && r <= '9')
}

// workOLID extracts the Open Library work id from a "/works/OL...W" key,
// returning "" for a key that is not a work reference. The bare OLID (OL...W) is
// what a Work's external_ids row records.
func workOLID(key string) string {
	key = strings.TrimSpace(key)
	const prefix = "/works/"
	if !strings.HasPrefix(key, prefix) {
		return ""
	}
	return strings.TrimPrefix(key, prefix)
}

// searchResponse is Open Library's /search.json body. Only the fields an
// enrichment needs are read; the many others are tolerated.
type searchResponse struct {
	Docs []searchDoc `json:"docs"`
}

type searchDoc struct {
	// Key is the work reference, "/works/OL...W".
	Key   string `json:"key"`
	Title string `json:"title"`
	// AuthorName is the authors, most-credited first; the first is taken as the
	// canonical author for a confident correction.
	AuthorName []string `json:"author_name"`
	// CoverID is the numeric cover id the cover image is fetched by. Zero means
	// Open Library holds no cover for this work.
	CoverID          int    `json:"cover_i"`
	CoverEditionKey  string `json:"cover_edition_key"`
	FirstPublishYear int    `json:"first_publish_year"`
}

var _ providers.EnrichProvider = (*Client)(nil)
