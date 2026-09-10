// Package opensubtitles is the first SubtitleProvider (M12 Phase 5): an adapter
// that fetches a subtitle for content the library already holds but has no
// caption for — neither a shipped sidecar nor an embedded track — from
// opensubtitles.com's REST API. It is the provider ADR-0085 named: the last
// resort after ADR-0084's shipped/embedded extraction, reached only when the
// bytes are genuinely nowhere.
//
// # Two secrets, and why (AuthTokenBasic)
//
// opensubtitles.com authenticates a SEARCH with an Api-Key header, but the
// /download endpoint that turns a chosen file into a temporary fetch URL — and
// spends the account's daily quota — needs a JWT the client mints from a
// username+password login. Neither secret alone is a working credential, which
// is the whole reason providers.AuthTokenBasic exists (ADR-0031, credential.go):
// an operator supplies an api-key, a username and a password, and how each goes
// on the wire (an Api-Key header, a bearer token from the login) is this client's
// business. The api-key travels in a header, never a URL, for tmdb's reason: a
// secret in a query string leaks into the first transport error that renders the
// request URL.
//
// # Search is cheap and cacheable, resolve spends the budget
//
// FindSubtitle and ResolveSubtitle are two calls because the service charges the
// resolve, not the search, against a daily download limit. So a search is rate
// limited and briefly cached (a backfill re-asks the same query as it retries),
// and a resolve reaches the service every time, minting one per-fetch link for
// the candidate a caller chose (ADR-0085's "the adapter ranks; the profile does
// not gate"). The JWT the resolve needs is minted lazily on the first download
// and reused until it is rejected, then minted once more.
//
// # It is never exercised in CI (ADR-0026)
//
// OpenSubtitles is an external service reached with a credential; per ADR-0026
// the real client is driven only against a recorded corpus over httptest, and
// this package's tests do exactly that. The corpus is synthesised from the
// published REST contract because CI has no key and none may be committed; the
// credentials are supplied at construction and never reach the corpus (auth
// travels in headers the replay harness does not match on).
package opensubtitles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/providers/ratelimit"
)

// defaultEndpoint is opensubtitles.com's REST base URL. Overridden by
// Options.Endpoint, which is how the fixture harness points the real client at
// an httptest server.
const defaultEndpoint = "https://api.opensubtitles.com/api/v1"

// defaultTimeout bounds one call. A search or a download-link mint is a small
// JSON round trip; generous but bounded, so a job holding a lease does not wait
// forever on a service that will never answer.
const defaultTimeout = 30 * time.Second

// maxBodyBytes bounds what will be read. A page of subtitle results is a few KB
// of JSON; this is a guard against a misdirected endpoint, not a real limit.
const maxBodyBytes = 8 << 20

// defaultMinInterval spaces requests to stay under the ~5 req/s ceiling — a hair
// over 200ms is ~4.5/s, comfortably below the limit with margin for clock skew.
const defaultMinInterval = 220 * time.Millisecond

// defaultCacheTTL is how long a search result stays fresh. Long enough that a
// backfill's retries of one want reuse the call, short enough that a subtitle
// uploaded moments ago is found on the next real search.
const defaultCacheTTL = 10 * time.Minute

// maxCandidates bounds a search response mapped out. A caller ranks the first
// handful; a query that matched hundreds of files is not one a caller reads
// through, and mapping them all would be work no one uses.
const maxCandidates = 50

// Options configure an OpenSubtitles client.
type Options struct {
	// Name is the provider's name from configuration — the operator's word, used
	// in health and routing.
	Name string
	// Endpoint is the API base URL. Empty means defaultEndpoint. Tests set it to
	// a fixture server's URL.
	Endpoint string
	// APIKey is the Api-Key that authenticates a search, revealed at construction
	// and sent as the Api-Key header. Never in a URL.
	APIKey string
	// Username and Password mint the JWT the /download endpoint needs. Revealed
	// at construction; the password never reaches a log or the corpus.
	Username string
	Password string
	// UserAgent identifies heyarr to the service, which REQUIRES a descriptive
	// one and throttles a client that omits it. Empty means a bare "heyarr".
	UserAgent string
	// HTTPClient is injected by tests. Nil means a client of this package's own
	// making.
	HTTPClient *http.Client
	// Now is the injected clock.
	Now func() time.Time
	// MinInterval overrides the request spacing. Zero means defaultMinInterval; a
	// negative value disables the limiter (tests that are not asserting spacing).
	MinInterval time.Duration
	// CacheTTL overrides the search cache window. Zero means defaultCacheTTL; a
	// negative value disables the cache.
	CacheTTL time.Duration
	// sleep is the limiter's sleep, injected only by tests so spacing is asserted
	// without a real wait. Nil means the production context-aware sleep.
	sleep func(ctx context.Context, d time.Duration) error
}

// Client is one OpenSubtitles subtitle provider. It satisfies
// providers.SubtitleProvider.
type Client struct {
	name      string
	endpoint  string
	apiKey    string
	username  string
	password  string
	userAgent string
	http      *http.Client
	now       func() time.Time

	limiter *ratelimit.RateLimiter
	cache   *searchCache

	// jwtMu guards the lazily-minted download token. login sets it; a rejected
	// token clears it so the next resolve mints a fresh one.
	jwtMu sync.Mutex
	jwt   string
}

// New builds an OpenSubtitles client.
func New(o Options) (*Client, error) {
	if strings.TrimSpace(o.Name) == "" {
		return nil, errors.New("opensubtitles: a provider needs a name")
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
	ttl := o.CacheTTL
	if ttl == 0 {
		ttl = defaultCacheTTL
	}
	return &Client{
		name:      o.Name,
		endpoint:  endpoint,
		apiKey:    o.APIKey,
		username:  o.Username,
		password:  o.Password,
		userAgent: userAgent,
		http:      httpClient,
		now:       now,
		limiter:   ratelimit.New(interval, now, o.sleep),
		cache:     newSearchCache(ttl, now),
	}, nil
}

// Name is the operator's name for this provider.
func (c *Client) Name() string { return c.name }

// Capabilities is what this provider does: it is a subtitle provider.
func (c *Client) Capabilities() []providers.Capability {
	return []providers.Capability{providers.CapabilitySubtitle}
}

// Check exercises the provider by fetching the public-but-authenticated
// /infos/languages, which requires only the Api-Key, and reports what it found.
// It EXERCISES rather than asserts (providers.Provider): an api-key that is
// configured but rejected must report unhealthy so work does not route here and
// then fail. The login is not exercised — it is minted lazily on the first
// download — so Check reports on the half every request needs.
func (c *Client) Check(ctx context.Context) providers.Health {
	at := c.now().UTC()
	var langs languagesResponse
	if err := c.get(ctx, c.endpoint+"/infos/languages", "languages", &langs); err != nil {
		return providers.Unhealthy(authDetail(err), at)
	}
	return providers.Healthy("v1", at)
}

// FindSubtitle returns the subtitle files matching the query, as neutral
// candidates (providers.SubtitleProvider). An empty result is the modelled
// "nothing matched" outcome, not an error.
//
// The query is served from the short-lived search cache when it is fresh, so a
// backfill's retries of one want do not re-spend a request; a miss is rate
// limited, fetched, mapped and cached.
func (c *Client) FindSubtitle(ctx context.Context, q providers.SubtitleQuery) ([]providers.SubtitleCandidate, error) {
	values := searchValues(q)
	key := values.Encode()
	if cached, ok := c.cache.get(key); ok {
		return cached, nil
	}

	var body searchResponse
	path := c.endpoint + "/subtitles?" + key
	if err := c.get(ctx, path, "search", &body); err != nil {
		return nil, err
	}

	out := make([]providers.SubtitleCandidate, 0, len(body.Data))
	for _, d := range body.Data {
		if len(out) >= maxCandidates {
			break
		}
		a := d.Attributes
		for _, f := range a.Files {
			if f.FileID == 0 {
				// A file with no id cannot be resolved — the whole point of a
				// candidate is an id a resolve acts on — so it is skipped rather
				// than surfaced as an unresolvable row.
				continue
			}
			out = append(out, providers.SubtitleCandidate{
				FileID:          strconv.FormatInt(f.FileID, 10),
				Language:        canonicalLang(a.Language),
				Release:         strings.TrimSpace(a.Release),
				HearingImpaired: a.HearingImpaired,
				DownloadCount:   a.DownloadCount,
				Format:          strings.ToLower(strings.TrimSpace(a.Format)),
			})
			if len(out) >= maxCandidates {
				break
			}
		}
	}
	c.cache.put(key, out)
	return out, nil
}

// ResolveSubtitle turns a candidate's FileID into the URL its bytes can be
// fetched from, plus what the quota has left (providers.SubtitleProvider). It is
// the call that spends a download against the account's budget.
//
// The download endpoint needs a JWT beside the Api-Key. The token is minted
// lazily and reused; a resolve that comes back 401 mints a fresh one ONCE and
// retries, because the ordinary reason a good token 401s is that it expired, and
// a single re-login distinguishes an expired token (recovers) from a rejected
// login (fails, and says so) without a retry loop.
func (c *Client) ResolveSubtitle(ctx context.Context, fileID string) (providers.SubtitleLink, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(fileID), 10, 64)
	if err != nil {
		return providers.SubtitleLink{}, fmt.Errorf("opensubtitles: file id %q is not numeric: %w", fileID, err)
	}

	link, err := c.download(ctx, id, false)
	if err != nil {
		var he *httpError
		if errors.As(err, &he) && he.status == http.StatusUnauthorized {
			// Token expired or absent: mint a fresh one and try once more. A
			// second 401 is a rejected LOGIN, not an expired token, and is
			// returned rather than retried.
			return c.download(ctx, id, true)
		}
		return providers.SubtitleLink{}, err
	}
	return link, nil
}

// download performs one /download call, minting the JWT first if needed (or if
// forceLogin, unconditionally). It is the shared body of the resolve's first
// attempt and its post-401 retry.
func (c *Client) download(ctx context.Context, fileID int64, forceLogin bool) (providers.SubtitleLink, error) {
	token, err := c.ensureToken(ctx, forceLogin)
	if err != nil {
		return providers.SubtitleLink{}, err
	}

	reqBody, err := json.Marshal(downloadRequest{FileID: fileID})
	if err != nil {
		return providers.SubtitleLink{}, fmt.Errorf("opensubtitles: encoding download request: %w", err)
	}

	var body downloadResponse
	if err := c.post(ctx, c.endpoint+"/download", "download", reqBody, token, &body); err != nil {
		return providers.SubtitleLink{}, err
	}
	if strings.TrimSpace(body.Link) == "" {
		return providers.SubtitleLink{}, fmt.Errorf("opensubtitles: download returned no link for file %d", fileID)
	}

	remaining := -1
	if body.Remaining != nil {
		remaining = *body.Remaining
	}
	return providers.SubtitleLink{
		URL:       secret.Value(strings.TrimSpace(body.Link)),
		FileName:  strings.TrimSpace(body.FileName),
		Remaining: remaining,
		ResetsAt:  parseResetTime(body.ResetTimeUTC),
	}, nil
}

// ensureToken returns a usable JWT, minting one via /login if none is held or if
// force is set. It is guarded so two concurrent resolves mint at most one token.
func (c *Client) ensureToken(ctx context.Context, force bool) (string, error) {
	c.jwtMu.Lock()
	defer c.jwtMu.Unlock()
	if c.jwt != "" && !force {
		return c.jwt, nil
	}
	// A forced re-login drops the stale token first, so a login failure does not
	// leave a token that will 401 again standing.
	c.jwt = ""

	// The password is serialised into the login request body ON PURPOSE: that is
	// what /login authenticates with, and OpenSubtitles requires a JSON body (a
	// form-encoded pair, as qBittorrent uses, is refused). This is a deliberate
	// write to the wire the service must receive, not the accidental leak into
	// output G117 exists to catch — the request is sent over TLS and this client
	// logs no request body.
	reqBody, err := json.Marshal(loginRequest{Username: c.username, Password: c.password}) //nolint:gosec // the login body must carry the password; it goes only to /login over TLS, never a log
	if err != nil {
		return "", fmt.Errorf("opensubtitles: encoding login request: %w", err)
	}
	var body loginResponse
	if err := c.post(ctx, c.endpoint+"/login", "login", reqBody, "", &body); err != nil {
		return "", err
	}
	if strings.TrimSpace(body.Token) == "" {
		return "", errors.New("opensubtitles: login returned no token")
	}
	c.jwt = body.Token
	return c.jwt, nil
}

// get performs a rate-limited, api-key-authenticated GET and decodes the JSON
// body. op is stamped into a non-200 error so a failed search, health check or
// download are told apart in a health detail or a log. The api-key travels only
// in the Api-Key header, never in path, so an error rendering the request URL
// cannot leak it.
func (c *Client) get(ctx context.Context, path, op string, into any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("opensubtitles: building %s request: %w", op, err)
	}
	c.setHeaders(req, "")
	return c.do(req, op, into)
}

// post performs a rate-limited POST with a JSON body. A non-empty bearer adds
// the JWT the /download endpoint needs beside the Api-Key; /login passes "".
func (c *Client) post(ctx context.Context, path, op string, reqBody []byte, bearer string, into any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("opensubtitles: building %s request: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setHeaders(req, bearer)
	return c.do(req, op, into)
}

// setHeaders puts the api-key, accept, user-agent and (when given) the bearer
// token on a request. The credentials are set exactly here, at the point they
// are handed to the request that must send them, and never reach the URL.
func (c *Client) setHeaders(req *http.Request, bearer string) {
	req.Header.Set("Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
}

// do sends a prepared request, reads the bounded body and decodes it, mapping a
// non-200 to an httpError that carries the status so a caller (and Check) can
// tell an auth failure from an outage.
func (c *Client) do(req *http.Request, op string, into any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opensubtitles: %s request failed: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("opensubtitles: reading %s response: %w", op, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &httpError{status: resp.StatusCode, op: op}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("opensubtitles: decoding %s response: %w", op, err)
	}
	return nil
}

// httpError is a non-200 from OpenSubtitles, carrying the status so a caller and
// Check can tell an auth failure from an outage or a spent quota.
type httpError struct {
	status int
	op     string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("opensubtitles: %s returned HTTP %d", e.op, e.status)
}

// authDetail turns a check error into a health detail that never leaks a
// credential. OpenSubtitles answers a rejected api-key with 401; a 403 is a
// disabled or over-quota account, which is still a "your credential will not
// work" answer rather than an outage.
func authDetail(err error) string {
	var he *httpError
	if errors.As(err, &he) {
		switch he.status {
		case http.StatusUnauthorized:
			return "the API key was rejected"
		case http.StatusForbidden:
			return "the account is forbidden (disabled or over quota)"
		}
	}
	return "could not reach OpenSubtitles"
}

// searchValues builds the /subtitles query from a SubtitleQuery, using only the
// fields the caller set. url.Values.Encode sorts the keys, so the encoded string
// is a stable cache key for the same query however it was assembled.
func searchValues(q providers.SubtitleQuery) neturl.Values {
	v := neturl.Values{}
	if langs := cleanLangs(q.Languages); langs != "" {
		v.Set("languages", langs)
	}
	if id := strings.TrimSpace(q.IMDBID); id != "" {
		v.Set("imdb_id", id)
	}
	if id := strings.TrimSpace(q.TMDBID); id != "" {
		v.Set("tmdb_id", id)
	}
	if q.Season > 0 {
		v.Set("season_number", strconv.Itoa(q.Season))
	}
	if q.Episode > 0 {
		v.Set("episode_number", strconv.Itoa(q.Episode))
	}
	if h := strings.TrimSpace(q.MovieHash); h != "" {
		v.Set("moviehash", strings.ToLower(h))
	}
	return v
}

// cleanLangs normalises the wanted languages into OpenSubtitles' comma-separated
// lower-case form, dropping blanks and preserving the caller's preference order.
func cleanLangs(in []string) string {
	out := make([]string, 0, len(in))
	for _, l := range in {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, ",")
}

// canonicalLang lower-cases and trims a language code the service reports, so a
// candidate's Language matches the ISO-639-1 codes a want carries.
func canonicalLang(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// parseResetTime reads OpenSubtitles' quota reset timestamp. It is sent as an
// RFC3339 UTC string; a missing or unparseable value is the zero time, distinct
// from any real reset.
func parseResetTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// The wire shapes, kept unexported: nothing outside this package should couple
// to OpenSubtitles' JSON. Each reads only the fields a fetch needs and tolerates
// the many others the service sends.

// searchResponse is the /subtitles body: matched subtitles under data, each
// carrying its files.
type searchResponse struct {
	Data []searchDatum `json:"data"`
}

type searchDatum struct {
	ID         string           `json:"id"`
	Attributes searchAttributes `json:"attributes"`
}

type searchAttributes struct {
	Language        string    `json:"language"`
	DownloadCount   int       `json:"download_count"`
	HearingImpaired bool      `json:"hearing_impaired"`
	Format          string    `json:"format"`
	Release         string    `json:"release"`
	Files           []subFile `json:"files"`
}

type subFile struct {
	// FileID is the value /download takes. OpenSubtitles sends it as a NUMBER;
	// this adapter renders it to the string a SubtitleCandidate carries.
	FileID   int64  `json:"file_id"`
	FileName string `json:"file_name"`
}

// loginRequest and loginResponse are the /login exchange: a username+password in,
// a JWT out.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token   string `json:"token"`
	BaseURL string `json:"base_url"`
	Status  int    `json:"status"`
}

// downloadRequest and downloadResponse are the /download exchange: a file id in,
// a temporary link and the quota counters out.
type downloadRequest struct {
	FileID int64 `json:"file_id"`
}

type downloadResponse struct {
	Link     string `json:"link"`
	FileName string `json:"file_name"`
	// Remaining is a pointer so "the service did not say" (nil → -1) is
	// distinguishable from a genuine zero downloads left.
	Remaining    *int   `json:"remaining"`
	ResetTimeUTC string `json:"reset_time_utc"`
}

// languagesResponse is /infos/languages, read only so Check's decode is honest;
// nothing in it is used beyond proving the api-key was accepted.
//
// The live API returns `data` as a FLAT ARRAY — {"data":[{"language_code":"en",…}]}
// — NOT an object with a nested `languages` field. The original struct
// (data.languages[]) was synthesised from the stoplight docs, not a live response,
// so against the real endpoint json.Unmarshal hit an array-into-struct type
// mismatch; do() returned that decode error and authDetail masked it as the
// generic "could not reach OpenSubtitles" (verified live 2026-09-10 from
// the reference host: the request is 200, only the decode failed).
type languagesResponse struct {
	Data []struct {
		LanguageCode string `json:"language_code"`
	} `json:"data"`
}

var _ providers.SubtitleProvider = (*Client)(nil)
