// Package tvdb is the first FeedProvider (M12): a metadata adapter that
// enumerates a TV series' episodes and air dates from TheTVDB v4 API.
//
// # Why TVDB, and why behind an interface
//
// A followed TV series needs a calendar — "which episodes should exist, and
// when did or will they air" — and TVDB is the *arr-ecosystem standard for
// accurate season/episode numbering and air dates, which is exactly what
// episode-following turns on. TMDB is a reasonable alternative with broader
// general metadata and friendlier API terms; it is a later, pluggable
// implementation of the SAME providers.FeedProvider interface (ADR-0058). This
// package hardcodes nothing that a second implementation would have to fight:
// it returns the neutral followed.FeedItem, and a followed source names TVDB by
// configuration, never in code.
//
// # It is never exercised in CI
//
// TVDB is an external service with a credential; per ADR-0026 the real client
// is driven only against recorded fixtures over httptest, and this package's
// tests do exactly that. The API key is supplied at construction (from a
// credential or an environment reference at the edge) and is never committed.
package tvdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// defaultEndpoint is TheTVDB v4 base URL. Overridden by Options.Endpoint, which
// is how the fixture harness points the real client at an httptest server.
const defaultEndpoint = "https://api4.thetvdb.com/v4"

// defaultTimeout bounds one call. A metadata lookup is a small JSON round trip;
// generous but bounded, so a poll job holding a lease does not wait forever on
// a service that will never answer.
const defaultTimeout = 30 * time.Second

// maxBodyBytes bounds what will be read. A season of episodes is a few KB of
// JSON; this is a guard against a misdirected endpoint, not a real limit.
const maxBodyBytes = 8 << 20

// maxPages bounds pagination so a wrong `links.next` cannot loop forever. A
// series with more than this many pages of episodes is not a series.
const maxPages = 100

// Options configure a TVDB client.
type Options struct {
	// Name is the provider's name from configuration — the operator's word,
	// used in health and routing.
	Name string
	// Endpoint is the API base URL. Empty means defaultEndpoint. Tests set it to
	// a fixture server's URL.
	Endpoint string
	// APIKey is the credential, revealed at construction by the caller (the
	// registry, from a credential or env reference) and never committed.
	APIKey string
	// HTTPClient is injected by tests. Nil means a client of this package's own
	// making.
	HTTPClient *http.Client
	// Now is the injected clock.
	Now func() time.Time
}

// Client is one TVDB metadata provider. It satisfies providers.FeedProvider.
type Client struct {
	name     string
	endpoint string
	apiKey   string
	http     *http.Client
	now      func() time.Time
}

// New builds a TVDB client.
func New(o Options) (*Client, error) {
	if strings.TrimSpace(o.Name) == "" {
		return nil, errors.New("tvdb: a provider needs a name")
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
		apiKey:   o.APIKey,
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

// Check exercises the provider by logging in, and reports what it found. It
// EXERCISES rather than asserts (providers.Provider): a key that is configured
// but rejected must report unhealthy so work does not route to it and then fail.
func (c *Client) Check(ctx context.Context) providers.Health {
	at := c.now().UTC()
	if _, err := c.login(ctx); err != nil {
		return providers.Unhealthy(loginDetail(err), at)
	}
	// TVDB's login does not report an API version; the major we target is v4,
	// which is fixed by the endpoint path rather than negotiated.
	return providers.Healthy("v4", at)
}

// Enumerate returns a series' episodes as neutral FeedItems. ref is the TVDB
// series id.
//
// It logs in, then walks the default season-order episode pages, mapping each
// episode to a FeedItem keyed "S%02dE%02d" — the source-stable key that dedupes
// an episode across polls. An episode with no usable season/episode number is
// skipped rather than projected under an ambiguous key.
func (c *Client) Enumerate(ctx context.Context, ref string) ([]followed.FeedItem, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("tvdb: a series id is required to enumerate episodes")
	}
	token, err := c.login(ctx)
	if err != nil {
		return nil, err
	}

	var items []followed.FeedItem
	seen := make(map[string]bool)
	for page := 0; page < maxPages; page++ {
		path := fmt.Sprintf("%s/series/%s/episodes/default?page=%d", c.endpoint, url(ref), page)
		var body episodesResponse
		if err := c.getJSON(ctx, path, token, &body); err != nil {
			return nil, err
		}
		for _, ep := range body.Data.Episodes {
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
				PublishedAt: parseAired(ep.Aired),
				Attributes: map[string]string{
					"season":          fmt.Sprintf("%d", *ep.SeasonNumber),
					"episode":         fmt.Sprintf("%d", *ep.Number),
					"tvdb_episode_id": fmt.Sprintf("%d", ep.ID),
				},
			})
		}
		if strings.TrimSpace(body.Links.Next) == "" || len(body.Data.Episodes) == 0 {
			break
		}
	}
	return items, nil
}

// login exchanges the API key for a bearer token (TVDB v4 POST /login).
func (c *Client) login(ctx context.Context) (string, error) {
	// G117: the API key MUST be serialized into the login body — that is
	// TheTVDB v4 auth contract, and this is the single place the key is sent,
	// revealed only to its intended destination. It never reaches a log or the
	// corpus (the fixtures are synthesised and key-free).
	reqBody, _ := json.Marshal(loginRequest{APIKey: c.apiKey}) //nolint:gosec // G117: the login body is the key's intended destination (TVDB v4 auth)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/login", strings.NewReader(string(reqBody)))
	if err != nil {
		return "", fmt.Errorf("tvdb: building login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("tvdb: login request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", fmt.Errorf("tvdb: reading login response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", &httpError{status: resp.StatusCode, op: "login"}
	}
	var body loginResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("tvdb: decoding login response: %w", err)
	}
	if strings.TrimSpace(body.Data.Token) == "" {
		return "", errors.New("tvdb: login succeeded but returned no token")
	}
	return body.Data.Token, nil
}

// getJSON performs an authenticated GET and decodes the JSON body.
func (c *Client) getJSON(ctx context.Context, path, token string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("tvdb: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tvdb: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("tvdb: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &httpError{status: resp.StatusCode, op: "episodes"}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("tvdb: decoding response: %w", err)
	}
	return nil
}

// httpError is a non-200 from TVDB, carrying the status so a caller (and Check)
// can tell an auth failure from an outage.
type httpError struct {
	status int
	op     string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("tvdb: %s returned HTTP %d", e.op, e.status)
}

// loginDetail turns a login error into a health detail that never leaks the key.
func loginDetail(err error) string {
	var he *httpError
	if errors.As(err, &he) && he.status == http.StatusUnauthorized {
		return "the API key was rejected"
	}
	return "could not reach TVDB"
}

// parseAired parses TVDB's "YYYY-MM-DD" air date. A missing or unparseable date
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

// url escapes a path segment minimally — a TVDB series id is numeric, but a
// caller-supplied ref must not be able to inject a path.
func url(seg string) string {
	return strings.ReplaceAll(strings.ReplaceAll(seg, "/", "%2F"), "?", "%3F")
}

// The wire shapes, kept unexported: nothing outside this package should couple
// to TVDB's JSON.

type loginRequest struct {
	APIKey string `json:"apikey"`
}

type loginResponse struct {
	Status string `json:"status"`
	Data   struct {
		Token string `json:"token"`
	} `json:"data"`
}

type episodesResponse struct {
	Status string `json:"status"`
	Data   struct {
		Episodes []episode `json:"episodes"`
	} `json:"data"`
	Links struct {
		Next string `json:"next"`
	} `json:"links"`
}

type episode struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Aired        string `json:"aired"`
	SeasonNumber *int   `json:"seasonNumber"`
	Number       *int   `json:"number"`
}
