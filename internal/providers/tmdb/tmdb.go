// Package tmdb is the second FeedProvider (M12): a metadata adapter that
// discovers TV series and enumerates their episodes and air dates from The
// Movie Database (TMDB) v3 API. It is the pluggable sibling ADR-0058 promised —
// a drop-in second implementation of providers.FeedProvider and
// providers.DiscoverySearcher behind CapabilityMetadata, so a deployment can
// discover and follow TV series through TMDB when TheTVDB is unavailable
// (thetvdb/v4-api#382) without the poll loop, the projection, or any caller
// learning that the service changed.
//
// # Why TMDB, and why behind the same interface as TVDB
//
// A followed TV series needs a calendar — "which episodes should exist, and
// when did or will they air". TVDB is the *arr-ecosystem standard for that;
// TMDB is the reasonable alternative ADR-0058 named, with broader general
// metadata and friendlier API terms. This package hardcodes nothing a third
// implementation would have to fight: it returns the neutral followed.FeedItem
// and the neutral providers.DiscoveryCandidate, and a followed source names its
// metadata provider by configuration, never in code.
//
// # Authentication: a v4 bearer token against the v3 endpoints
//
// TMDB offers two credential shapes: a v3 api_key sent as a query parameter, and
// a v4 read access token sent as an Authorization: Bearer header. Both are ONE
// opaque secret, so both are providers.AuthToken (ADR-0031) — the choice is only
// how the token goes on the wire, which is this client's business. This adapter
// sends the v4 read access token as a bearer HEADER, deliberately, for the same
// reason TVDB does: a secret in a query parameter travels into the request URL,
// and a transport error or a debug log renders that URL verbatim — so an api_key
// in the query would leak into a log the first time a request failed. A header
// keeps the credential out of every URL, and out of the fixture corpus's matched
// request paths (the replay harness matches method+path, never a header), which
// is what lets the synthesised corpus carry no key at all.
//
// An operator therefore supplies a TMDB v4 "API Read Access Token"
// (themoviedb.org → Settings → API), not the v3 key, as the provider's token.
//
// # It is never exercised in CI
//
// TMDB is an external service reached with a credential; per ADR-0026 the real
// client is driven only against a recorded corpus over httptest, and this
// package's tests do exactly that. The corpus is synthesised from the published
// v3 contract because CI has no key and none may be committed; the token is
// supplied at construction (from a credential or an environment reference at the
// edge) and is never committed.
//
// # Movies are deliberately out of scope
//
// TMDB indexes movies as well as TV, but heyarr's discovery and following model
// is feed-shaped: a DiscoveryCandidate.Type is a followed.Type, a FeedProvider
// enumerates the items WITHIN a followed source over time, and neither has a
// place for a movie — a one-off want (want_content), not a subscription with a
// calendar. Emitting a movie candidate would need a new followed.Type and a
// want-scoped discovery path, which is a catalog/acquisition-side change, not a
// provider addition (see ADR-0077). So this adapter serves tv_series only, the
// half that slots in behind the existing interface with no domain change.
package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// defaultEndpoint is TMDB's v3 base URL. Overridden by Options.Endpoint, which
// is how the fixture harness points the real client at an httptest server.
const defaultEndpoint = "https://api.themoviedb.org/3"

// defaultTimeout bounds one call. A metadata lookup is a small JSON round trip;
// generous but bounded, so a poll job holding a lease does not wait forever on a
// service that will never answer.
const defaultTimeout = 30 * time.Second

// maxBodyBytes bounds what will be read. A season of episodes is a few KB of
// JSON; this is a guard against a misdirected endpoint, not a real limit.
const maxBodyBytes = 8 << 20

// maxSeasons bounds how many season pages a single enumeration will fetch, so a
// series with an implausible season count cannot turn one poll into thousands of
// requests. A series with more than this many seasons is not a series.
const maxSeasons = 200

// maxSearchResults bounds a discovery response. TMDB's /search/tv returns the
// most relevant matches first, twenty to a page, and a person choosing a series
// to follow reads the first handful; this adapter asks for the first page only,
// which is the list a person actually scrolls.
const maxSearchResults = 25

// Options configure a TMDB client.
type Options struct {
	// Name is the provider's name from configuration — the operator's word, used
	// in health and routing.
	Name string
	// Endpoint is the API base URL. Empty means defaultEndpoint. Tests set it to
	// a fixture server's URL.
	Endpoint string
	// Token is the credential — a TMDB v4 read access token — revealed at
	// construction by the caller (the registry, from a credential or env
	// reference) and never committed. Sent as an Authorization: Bearer header.
	Token string
	// HTTPClient is injected by tests. Nil means a client of this package's own
	// making.
	HTTPClient *http.Client
	// Now is the injected clock.
	Now func() time.Time
}

// Client is one TMDB metadata provider. It satisfies providers.FeedProvider and
// providers.DiscoverySearcher.
type Client struct {
	name     string
	endpoint string
	token    string
	http     *http.Client
	now      func() time.Time
}

// New builds a TMDB client.
func New(o Options) (*Client, error) {
	if strings.TrimSpace(o.Name) == "" {
		return nil, errors.New("tmdb: a provider needs a name")
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
	return &Client{
		name:     o.Name,
		endpoint: endpoint,
		token:    o.Token,
		http:     httpClient,
		now:      now,
	}, nil
}

// Name is the operator's name for this provider.
func (c *Client) Name() string { return c.name }

// Capabilities is what this provider does: it is a metadata feed provider.
func (c *Client) Capabilities() []providers.Capability {
	return []providers.Capability{providers.CapabilityMetadata}
}

// ServesType reports that this adapter enumerates tv_series sources. Movies are
// out of scope (see the package doc); TMDB's other content is not a followed
// source type.
func (c *Client) ServesType(t followed.Type) bool { return t == followed.TypeTVSeries }

// Check exercises the provider by fetching TMDB's public /configuration, and
// reports what it found. It EXERCISES rather than asserts (providers.Provider):
// a token that is configured but rejected must report unhealthy so work does not
// route to it and then fail.
func (c *Client) Check(ctx context.Context) providers.Health {
	at := c.now().UTC()
	var cfg configurationResponse
	if err := c.get(ctx, c.endpoint+"/configuration", "configuration", &cfg); err != nil {
		return providers.Unhealthy(authDetail(err), at)
	}
	// TMDB's /configuration does not report an API version; the major we target
	// is v3, fixed by the endpoint path rather than negotiated.
	return providers.Healthy("v3", at)
}

// Enumerate returns a series' episodes as neutral FeedItems. ref is the TMDB
// series id.
//
// Unlike TVDB's single paginated episodes endpoint, TMDB has no "all episodes"
// call: the series detail lists the seasons, and each season is fetched for its
// episodes. So this reads /tv/{id} for the season list, then walks the seasons
// in season-number order, mapping each episode to a FeedItem keyed "S%02dE%02d"
// — the source-stable key that dedupes an episode across polls. An episode with
// no usable season/episode number is skipped rather than projected under an
// ambiguous key.
func (c *Client) Enumerate(ctx context.Context, ref string) ([]followed.FeedItem, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("tmdb: a series id is required to enumerate episodes")
	}

	var detail tvDetail
	if err := c.get(ctx, fmt.Sprintf("%s/tv/%s", c.endpoint, pathSeg(ref)), "series", &detail); err != nil {
		return nil, err
	}

	// Deterministic order out, so per-item wants are not re-projected on every
	// poll: season-number ascending, then episode order within a season.
	seasons := append([]seasonRef(nil), detail.Seasons...)
	sort.SliceStable(seasons, func(i, j int) bool { return seasons[i].SeasonNumber < seasons[j].SeasonNumber })

	var items []followed.FeedItem
	seen := make(map[string]bool)
	for i, s := range seasons {
		if i >= maxSeasons {
			break
		}
		var season seasonDetail
		path := fmt.Sprintf("%s/tv/%s/season/%d", c.endpoint, pathSeg(ref), s.SeasonNumber)
		if err := c.get(ctx, path, "season", &season); err != nil {
			return nil, err
		}
		for _, ep := range season.Episodes {
			if ep.SeasonNumber == nil || ep.Number == nil {
				// An episode without a season/episode number cannot be given a
				// stable key, so it cannot be deduped or projected. Skipped, not
				// guessed.
				continue
			}
			key := fmt.Sprintf("S%02dE%02d", *ep.SeasonNumber, *ep.Number)
			if seen[key] {
				continue
			}
			seen[key] = true
			items = append(items, followed.FeedItem{
				Key:         key,
				Title:       strings.TrimSpace(ep.Name),
				PublishedAt: parseAired(ep.AirDate),
				Attributes: map[string]string{
					"season":          fmt.Sprintf("%d", *ep.SeasonNumber),
					"episode":         fmt.Sprintf("%d", *ep.Number),
					"tmdb_episode_id": fmt.Sprintf("%d", ep.ID),
				},
			})
		}
	}
	return items, nil
}

// Discover resolves a free-text query to candidate TV series, INCLUDING ones the
// library does not yet hold (#451). It satisfies providers.DiscoverySearcher.
//
// It asks TMDB v3's /search/tv for series matching the query, mapping each hit
// to a neutral providers.DiscoveryCandidate carrying the TMDB series id — so a
// caller turns a title into a follow in one step. /search/tv scopes the search
// to what this adapter can actually follow: a movie or a person hit would be a
// candidate no follow flow could act on.
//
// An empty result is the modelled "nothing matched" outcome, not an error; an
// error is a call that could not be made, which the caller must see rather than
// read as an empty catalogue.
func (c *Client) Discover(ctx context.Context, query string) ([]providers.DiscoveryCandidate, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("tmdb: a query is required to discover series")
	}

	// page=1 only: TMDB returns the most relevant matches first, and a follow
	// picker reads the first page. api_key is NOT in the query — the token is a
	// bearer header (see the package doc) — so the query carries only the search.
	path := fmt.Sprintf("%s/search/tv?query=%s&page=1", c.endpoint, queryEscape(query))
	var body searchResponse
	if err := c.get(ctx, path, "search", &body); err != nil {
		return nil, err
	}

	out := make([]providers.DiscoveryCandidate, 0, len(body.Results))
	for _, hit := range body.Results {
		if hit.ID == 0 {
			// A hit with no TMDB id cannot be followed — the whole point of a
			// candidate is an id a follow can act on — so it is skipped rather
			// than surfaced as an unfollowable row. (Every real TMDB hit has a
			// positive id; a zero id is the absent-id case.)
			continue
		}
		if len(out) >= maxSearchResults {
			break
		}
		out = append(out, providers.DiscoveryCandidate{
			Title:      strings.TrimSpace(hit.Name),
			Year:       parseYearFromDate(hit.FirstAirDate),
			ExternalID: strconv.FormatInt(hit.ID, 10),
			Type:       followed.TypeTVSeries,
			Overview:   strings.TrimSpace(hit.Overview),
		})
	}
	return out, nil
}

// get performs an authenticated GET and decodes the JSON body. op is stamped
// into a non-200 error so a failed discovery, enumeration or health check are
// told apart in a health detail or a log. The token travels only in the
// Authorization header, never in path, so an error rendering the request URL
// cannot leak it.
func (c *Client) get(ctx context.Context, path, op string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("tmdb: building %s request: %w", op, err)
	}
	// The bearer token is TMDB v4 read-access auth on the v3 endpoints, revealed
	// only here at the point it is handed to the request that must send it. It
	// never reaches a log or the corpus (the fixtures are synthesised and
	// key-free).
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tmdb: %s request failed: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("tmdb: reading %s response: %w", op, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &httpError{status: resp.StatusCode, op: op}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("tmdb: decoding %s response: %w", op, err)
	}
	return nil
}

// httpError is a non-200 from TMDB, carrying the status so a caller (and Check)
// can tell an auth failure from an outage.
type httpError struct {
	status int
	op     string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("tmdb: %s returned HTTP %d", e.op, e.status)
}

// authDetail turns a check error into a health detail that never leaks the
// token. TMDB answers a rejected token with 401.
func authDetail(err error) string {
	var he *httpError
	if errors.As(err, &he) && he.status == http.StatusUnauthorized {
		return "the API token was rejected"
	}
	return "could not reach TMDB"
}

// parseAired parses TMDB's "YYYY-MM-DD" air date. A missing or unparseable date
// is the zero time, which is distinct from any real date.
func parseAired(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// parseYearFromDate reads the year out of TMDB's "YYYY-MM-DD" first-air date,
// which TMDB sends as a string and sometimes leaves empty. A missing or
// unparseable date is year zero, distinct from any real year.
func parseYearFromDate(s string) int {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return 0
	}
	y, err := strconv.Atoi(s[:4])
	if err != nil {
		return 0
	}
	return y
}

// pathSeg escapes a path segment minimally — a TMDB series id is numeric, but a
// caller-supplied ref must not be able to inject a path.
func pathSeg(seg string) string {
	return strings.ReplaceAll(strings.ReplaceAll(seg, "/", "%2F"), "?", "%3F")
}

// queryEscape escapes a free-text discovery query for a URL query parameter. A
// caller's query is arbitrary text — spaces, ampersands, unicode — so unlike the
// numeric series id pathSeg guards, it needs full query-string escaping.
func queryEscape(s string) string {
	return neturl.QueryEscape(s)
}

// The wire shapes, kept unexported: nothing outside this package should couple
// to TMDB's JSON. Each reads only the fields a follow needs and tolerates the
// many others TMDB sends.

// searchResponse is TMDB v3's /search/tv body. The hits are a flat array under
// results.
type searchResponse struct {
	Results []searchHit `json:"results"`
}

type searchHit struct {
	// ID is the numeric TMDB series id — the value a follow acts on. TMDB sends
	// it as a NUMBER (unlike TVDB's string), and this adapter renders it to the
	// string a DiscoveryCandidate carries.
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// FirstAirDate is "YYYY-MM-DD" and sometimes empty.
	FirstAirDate string `json:"first_air_date"`
	Overview     string `json:"overview"`
}

// tvDetail is TMDB v3's /tv/{id} body. Only the season list is read — it is the
// index Enumerate walks to fetch each season's episodes.
type tvDetail struct {
	ID      int64       `json:"id"`
	Name    string      `json:"name"`
	Seasons []seasonRef `json:"seasons"`
}

type seasonRef struct {
	SeasonNumber int `json:"season_number"`
}

// seasonDetail is TMDB v3's /tv/{id}/season/{n} body.
type seasonDetail struct {
	SeasonNumber int       `json:"season_number"`
	Episodes     []episode `json:"episodes"`
}

type episode struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	AirDate string `json:"air_date"`
	// SeasonNumber and Number are pointers so an episode that omits either — one
	// that cannot be given a stable key — is distinguishable from episode zero
	// and skipped rather than mis-keyed.
	SeasonNumber *int `json:"season_number"`
	Number       *int `json:"episode_number"`
}

// configurationResponse is TMDB v3's /configuration body. Nothing in it is read
// — the health check cares only that the token was accepted — but a concrete
// type keeps get's decode honest rather than discarding the body.
type configurationResponse struct {
	Images struct {
		BaseURL string `json:"base_url"`
	} `json:"images"`
}
