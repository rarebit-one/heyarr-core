package indexers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// defaultTimeout bounds one call to an indexer.
//
// An indexer proxies trackers, several of them, some of which are slow or
// down — so this is generous by the standards of an HTTP client and still
// bounded, because a search job holding a lease waiting on a tracker that
// will never answer is a job that has stopped being durable.
const defaultTimeout = 60 * time.Second

// maxAttempts bounds retrying.
//
// Three: the first, and two more. A rate limit that has not cleared by then is
// not a blip, and continuing to press an indexer that is asking for room is
// how an operator's key gets banned from a tracker they care about.
const maxAttempts = 3

// baseBackoff is the first pause between attempts, doubled each time.
const baseBackoff = 2 * time.Second

// maxBodyBytes bounds what will be read from an indexer.
//
// A search can legitimately return a megabyte of XML — the captured corpus
// has a 144 KB one from a query with thirty results, and a hundred results
// with descriptions is several times that. This is a guard against a
// misdirected endpoint streaming something unbounded, not a limit any real
// response should approach.
const maxBodyBytes = 32 << 20

// ---------------------------------------------------------------------------
// The capabilities cache (#131). Four decisions, recorded here because every
// one of them is the difference between a saved round trip and an indexer
// that is wrong for a quarter of an hour.
// ---------------------------------------------------------------------------

// capsTTL is how long a capabilities document is believed without asking again.
//
// DECISION 1 — TEN MINUTES, and the unit is the whole argument. An indexer's
// capabilities change by HUMAN ACTION: somebody ticks "movie search" in a web
// UI, adds a tracker, or swaps Jackett for Prowlarr. Nothing changes them on a
// timescale of seconds, so seconds would spend requests to observe nothing;
// and nothing leaves them changed for a day without a person knowing they
// changed something, so hours would mean an operator who has just fixed their
// indexer watching Heyarr refuse for the rest of the afternoon.
//
// Ten minutes is the longest an operator should have to wonder, and it is a
// BACKSTOP rather than the primary freshness mechanism: the health job runs
// continuously and refreshes this cache every time it checks (see Check), so
// on a running node the entry is almost always fresher than the TTL requires.
// The TTL is what covers a node whose health job has not come round yet.
const capsTTL = 10 * time.Minute

// Options configure a Torznab client.
type Options struct {
	// Name is the provider's name from configuration — the operator's word,
	// used in health, in routing and in every candidate's Provider field.
	Name string
	// Endpoint is the Torznab API path, whole.
	//
	// The WHOLE path, because the two servers that serve this protocol do not
	// agree on its shape: one is /<indexer-id>/api, the other is
	// /api/v2.0/indexers/<id>/results/torznab/api. Composing it from a host
	// and a convention would be this client knowing which product it is
	// talking to, which is the one thing ADR-0028 says it must not know.
	Endpoint string
	// APIKey is the credential. Sent as the apikey query parameter, which is
	// what the protocol specifies.
	APIKey string
	// Capabilities is what configuration says this provider does.
	Capabilities []providers.Capability
	// HTTPClient is injected by tests. Nil means a client of this package's
	// own making — callers outside a test have no business supplying one, and
	// the registry's contract has no way to.
	HTTPClient *http.Client
	// Now is the injected clock.
	Now func() time.Time
	// Sleep is how backoff waits, injected so a retry test does not.
	Sleep func(context.Context, time.Duration) error
}

// Client is one Torznab indexer.
type Client struct {
	name     string
	endpoint string
	apiKey   string
	caps     []providers.Capability
	http     *http.Client
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error

	// capsMu guards the cached handshake below.
	//
	// A mutex rather than an atomic value because the document and the time it
	// was obtained are ONE fact and must be read as one: a reader that saw a
	// new document with an old timestamp would refresh something that had just
	// been refreshed, and one that saw an old document with a new timestamp
	// would trust it for another full TTL.
	//
	// It is NOT held across the HTTP call. Two searches starting together may
	// therefore both handshake once — which costs one extra request in a rare
	// race, where holding the lock would make every search on this indexer
	// queue behind whichever one is currently waiting on a slow tracker.
	capsMu sync.Mutex
	// capsDoc is the last capabilities document this client obtained, and
	// capsAt is when it obtained it. Nil means nothing has ever handshaked.
	//
	// DECISION 2 — THE CACHE LIVES PER CLIENT, IN MEMORY, AND NOWHERE ELSE.
	// Said here rather than left for the next reader to work out from the
	// absence of anything else: a Client is built per configured provider per
	// process, so the controller and the worker each hold their own, and
	// `heyarr all` holds two of them. That is deliberate rather than tolerated
	// — sharing it would mean a cache in the database, which is a row two
	// roles write and neither owns, to save one small request against a
	// service that is already proxying trackers. The visible consequence is
	// that the two roles can briefly disagree about what an indexer can do,
	// bounded by capsTTL, and every use of that disagreement is a search that
	// is refused rather than a search that is wrong.
	capsDoc *caps
	capsAt  time.Time
}

// Compile-time proof that this satisfies the registry's contracts.
var (
	_ providers.Provider = (*Client)(nil)
	_ providers.Indexer  = (*Client)(nil)
)

// New builds a client, refusing configuration that cannot work.
func New(o Options) (*Client, error) {
	if strings.TrimSpace(o.Name) == "" {
		return nil, errors.New("indexers: a provider needs a name")
	}
	if strings.TrimSpace(o.Endpoint) == "" {
		return nil, fmt.Errorf("indexers: %q has no endpoint", o.Name)
	}
	if _, err := url.Parse(o.Endpoint); err != nil {
		return nil, fmt.Errorf("indexers: %q has an endpoint that is not a URL: %w", o.Name, err)
	}
	c := &Client{
		name:     o.Name,
		endpoint: strings.TrimRight(o.Endpoint, "?&"),
		apiKey:   o.APIKey,
		caps:     o.Capabilities,
		http:     o.HTTPClient,
		now:      o.Now,
		sleep:    o.Sleep,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: defaultTimeout}
	}
	if c.now == nil {
		c.now = func() time.Time { return time.Now().UTC() }
	}
	if c.sleep == nil {
		c.sleep = sleepOrDone
	}
	if len(c.caps) == 0 {
		c.caps = []providers.Capability{providers.CapabilityIndexer}
	}
	return c, nil
}

// Name is the operator's name for this provider.
func (c *Client) Name() string { return c.name }

// Capabilities is what this provider can do.
func (c *Client) Capabilities() []providers.Capability {
	return append([]providers.Capability(nil), c.caps...)
}

// Check exercises the indexer with the protocol's own handshake.
//
// `t=caps` rather than a bare GET, and that is not ceremony: it is the one
// request that makes the indexer say who it is and what it can search, so a
// provider that answers it is provably usable rather than merely reachable.
// The same shape as reading Transmission's rpc-version.
//
// Returns a Health rather than an error because "unreachable" is a REPORT.
func (c *Client) Check(ctx context.Context) providers.Health {
	// ONE attempt, deliberately.
	//
	// A health check EXERCISES and REPORTS (ADR-0025). Retrying inside it
	// would make the health job hold its lease through a full backoff against
	// every unreachable provider, and would smooth over exactly the flapping
	// an operator is asking about when they read this endpoint. "It answered
	// when asked" is the observation; a search is where persistence belongs.
	//
	// It also REFRESHES THE CAPABILITIES CACHE, which is decision 3 below and
	// is the reason this reads refreshCapabilities rather than capabilities.
	doc, err := c.refreshCapabilities(ctx, 1)
	if err != nil {
		return providers.Unhealthy(c.detailFor(err), c.now())
	}

	version := strings.TrimSpace(doc.Server.Version)
	if version == "" {
		// Both servers measured leave it out. Not unhealthy — an indexer that
		// works and does not version itself is common, and refusing it would
		// be this client inventing a requirement the protocol does not make.
		version = "unreported"
	}
	// The product is reported, never branched on. An operator answering "why
	// did acquisitions stop after I upgraded" wants to know which server this
	// is; the code above must not care.
	detail := "reachable"
	if title := strings.TrimSpace(doc.Server.Title); title != "" {
		detail = "reachable — " + title
	}
	h := providers.Healthy(version, c.now())
	h.Detail = detail
	return h
}

// detailFor renders an error as something an operator can act on, and never
// as something that contains a credential.
func (c *Client) detailFor(err error) string {
	var perr *ProtocolError
	if errors.As(err, &perr) {
		if perr.Code == errCodeInvalidKey {
			// Named as configuration explicitly. This is the failure that
			// otherwise presents as "searches return nothing", and the whole
			// point of reading the error document is being able to say this.
			return "the API key was rejected — a configuration problem, not a transient one"
		}
		return fmt.Sprintf("the indexer refused: %s (error %d)", perr.Description, perr.Code)
	}
	if errors.Is(err, ErrNotTorznab) {
		return "the endpoint did not answer with Torznab XML — check the URL"
	}
	if errors.Is(err, ErrRateLimited) {
		return "the indexer is rate limiting"
	}
	if errors.Is(err, ErrUpstream) {
		return "the indexer answered with a server error"
	}
	// Deliberately NOT err.Error(): a transport error's text can contain the
	// request URL, and the request URL carries the API key.
	return "unreachable"
}

// capabilities performs the t=caps handshake.
func (c *Client) capabilities(ctx context.Context, attempts int) (*caps, error) {
	doc, err := c.call(ctx, url.Values{"t": []string{"caps"}}, attempts)
	if err != nil {
		return nil, err
	}
	got, ok := doc.(*caps)
	if !ok {
		return nil, fmt.Errorf("%w: asking for capabilities returned a %T", ErrNotTorznab, doc)
	}
	return got, nil
}

// cachedCapabilities is the handshake's answer, asking the indexer only when
// what it already holds is too old to be trusted.
//
// The handshake itself is NOT optional and is not made optional here. ADR-0028
// makes it the thing that turns a silent empty result into an honest refusal:
// an indexer that cannot search the wanted content type is REPORTED as unable
// to rather than queried and found wanting. What is cached is its ANSWER; the
// answer still decides every search, from the cache exactly as from the wire.
func (c *Client) cachedCapabilities(ctx context.Context, attempts int) (*caps, error) {
	if doc, at, ok := c.peekCapabilities(); ok && c.now().Sub(at) < capsTTL {
		return doc, nil
	}

	doc, err := c.refreshCapabilities(ctx, attempts)
	if err == nil {
		return doc, nil
	}

	// DECISION 4 — A STALE ENTRY IS SERVED WHEN THE REFRESH FAILED FOR A
	// REASON THAT MAY NOT STILL BE TRUE, AND ONLY THEN.
	//
	// The two halves are both load-bearing.
	//
	// Served during a blip: an indexer restarting, rate limiting, or behind a
	// proxy that returned a 502 is a service whose CAPABILITIES have not
	// changed — nobody reconfigured anything, it just did not answer this
	// second. Refusing the search there would turn a momentary handshake
	// failure into a failed search, when the search itself might well
	// succeed. If it does not, it fails on its own merits with its own error,
	// which is the better message anyway.
	//
	// NOT served when the failure will still be true next time: a rejected API
	// key, an endpoint that is not Torznab, an indexer declining the function.
	// Those are configuration, they do not clear on their own, and proceeding
	// on a remembered document would spend a second request to arrive at the
	// same refusal with a worse explanation. The test for "may not still be
	// true" is `retryable`, which is already this file's word for exactly that
	// distinction — it decides whether `call` tries again — so the cache and
	// the retry loop cannot drift into disagreeing about what transient means.
	//
	// A stale entry is deliberately NOT evicted on a failed refresh. The
	// failure is the case the stale entry exists for, and discarding it would
	// leave the next search with nothing at precisely the moment it is needed.
	if stale, _, ok := c.peekCapabilities(); ok && retryable(err) {
		return stale, nil
	}
	return nil, err
}

// refreshCapabilities performs the handshake and replaces what is cached.
//
// DECISION 3 — THE HEALTH CHECK IS THE INVALIDATION.
//
// Check already exercises `t=caps` (#99), so having it write the cache costs
// nothing and buys two things. The two can never be inconsistent: what an
// operator reads from GET /api/v1/providers is the same document the next
// search will use, rather than a second observation of an indexer that agrees
// with the first only by luck. And an operator who has just reconfigured their
// indexer has a FORCE-REFRESH PATH that already exists and is already
// documented — run the health check — instead of waiting out a TTL or
// restarting the process, which is the workaround this issue was written to
// avoid.
//
// The cache is written only on SUCCESS. A failed check leaves what is there
// alone, for the reason recorded in cachedCapabilities.
func (c *Client) refreshCapabilities(ctx context.Context, attempts int) (*caps, error) {
	doc, err := c.capabilities(ctx, attempts)
	if err != nil {
		return nil, err
	}
	c.capsMu.Lock()
	defer c.capsMu.Unlock()
	c.capsDoc = doc
	c.capsAt = c.now()
	return doc, nil
}

// peekCapabilities reads what is cached without asking the indexer anything.
func (c *Client) peekCapabilities() (*caps, time.Time, bool) {
	c.capsMu.Lock()
	defer c.capsMu.Unlock()
	if c.capsDoc == nil {
		return nil, time.Time{}, false
	}
	return c.capsDoc, c.capsAt, true
}

// ErrUnsupportedContentType is an indexer that cannot search for what is
// wanted.
//
// Reported rather than attempted. ADR-0028: an indexer that cannot search the
// content type being wanted should be said to be unable to, rather than
// queried and found wanting — the difference between "this indexer does not
// do music" and "no music was found", which look identical in a result set
// and lead to completely different actions.
var ErrUnsupportedContentType = errors.New("the indexer cannot search this content type")

// Search asks the indexer for releases (§59, §60).
func (c *Client) Search(ctx context.Context, q providers.Query) ([]acquisition.ReleaseCandidate, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}

	// The handshake first, and its answer decides the request.
	//
	// It buys the difference between an honest refusal and a silent empty
	// result, so it happens on every search — from the cache when one is
	// fresh, from the indexer when it is not. What is saved is the round
	// trip, never the check: functionFor below reads a cached document
	// exactly as it reads a freshly-fetched one, and refuses on it just the
	// same. See capsTTL for how long "fresh" is and why.
	doc, err := c.cachedCapabilities(ctx, maxAttempts)
	if err != nil {
		return nil, err
	}
	function, err := functionFor(doc, q.ContentType)
	if err != nil {
		return nil, err
	}

	params := url.Values{
		"t": []string{function},
		"q": []string{q.Title},
	}
	if q.Year > 0 {
		// Appended to the query rather than sent as its own parameter: the
		// captured servers advertise supportedParams="q" and nothing else, so
		// a year parameter would be silently ignored by one and honoured by
		// another — which is a difference in results nobody could see.
		params.Set("q", fmt.Sprintf("%s %d", q.Title, q.Year))
	}
	if q.Limit > 0 {
		params.Set("limit", fmt.Sprint(q.Limit))
	}

	answer, err := c.call(ctx, params, maxAttempts)
	if err != nil {
		return nil, err
	}
	results, ok := answer.(*feed)
	if !ok {
		return nil, fmt.Errorf("%w: searching returned a %T", ErrNotTorznab, answer)
	}
	return c.candidates(results), nil
}

// functionFor chooses the Torznab search function for a content type, and
// refuses when the indexer says it cannot.
func functionFor(doc *caps, contentType string) (string, error) {
	// A content type nobody named means the general search, which every
	// Torznab indexer must implement.
	specific := ""
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "":
	case "movie":
		specific = "movie"
	case "series", "tv":
		specific = "tvsearch"
	case "music":
		specific = "music"
	case "book":
		specific = "book"
	default:
		// An unknown content type falls back to the general search rather
		// than failing. §12's set may grow, and a provider refusing a content
		// type it has merely not heard of would make adding one a change
		// here too.
	}

	available := map[string]bool{
		"movie":    doc.Searching.MovieSearch.supported(),
		"tvsearch": doc.Searching.TVSearch.supported(),
		"music":    doc.Searching.MusicSearch.supported(),
		"book":     doc.Searching.BookSearch.supported(),
	}
	if specific != "" && available[specific] {
		return specific, nil
	}

	// The general search is the fallback, and only if the indexer advertises
	// it. Both captured servers advertise search=yes with every specific
	// search set to no, so this is the ordinary path rather than a corner.
	if doc.Searching.Search.supported() {
		return "search", nil
	}
	if specific != "" {
		return "", fmt.Errorf("%w: it advertises neither %s nor a general search",
			ErrUnsupportedContentType, specific)
	}
	return "", fmt.Errorf("%w: it advertises no general search", ErrUnsupportedContentType)
}

// candidates maps a feed onto the domain's values.
func (c *Client) candidates(f *feed) []acquisition.ReleaseCandidate {
	out := make([]acquisition.ReleaseCandidate, 0, len(f.Channel.Items))
	for _, i := range f.Channel.Items {
		title := strings.TrimSpace(i.Title)
		if title == "" {
			// A release with no name cannot be explained to anybody in §63's
			// output, and it cannot be told apart from another nameless one.
			continue
		}
		out = append(out, acquisition.ReleaseCandidate{
			ID:         candidateID(i),
			Title:      title,
			Provider:   c.name,
			Attributes: attributesOf(i),
			Source:     sourceOf(i),
		})
	}
	// Sorted by identity, NOT left in the order the server sent.
	//
	// §63's determinism guarantee is only as good as the ordering of what it
	// is handed: an indexer is free to return the same releases in a
	// different order on the next call — it proxies several trackers and
	// merges them — and a tie broken by position would then select a
	// different release for no reason anybody could see. A stably-ordered tie
	// looks exactly like a working system, which is why this line matters
	// more than it looks.
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// candidateID is stable for a release and independent of where it appeared.
//
// Preference order: the infohash, which IS the release's identity to
// BitTorrent and is the same value from any indexer that carries it; then the
// guid, which the protocol says is unique per item.
//
// Both are HASHED rather than used directly, and that is a privacy decision
// rather than a tidiness one. A guid is frequently a magnet URI, and a magnet
// URI from a private tracker carries a passkey that identifies a person. This
// id goes into the database, into API responses and into §63's explanations —
// so putting a raw guid in it would scatter a credential across every one of
// them.
func candidateID(i item) string {
	if hash, ok := i.attr("infohash"); ok {
		return "infohash:" + strings.ToLower(strings.TrimSpace(hash))
	}
	if guid := strings.TrimSpace(i.GUID); guid != "" {
		return "guid:" + digest(guid)
	}
	// Nothing identifying at all. The title and size together are weak, and
	// weak is still better than a position in a response: two parses of the
	// same document produce the same id, which is the property §63 needs.
	return "release:" + digest(strings.TrimSpace(i.Title)+"\x00"+i.Size)
}

// sourceOf is what to hand a download client to fetch this release.
//
// Preference order, and it is about how much the value can be trusted to be a
// direct fetch rather than a web page:
//
//	magneturl attr  torznab's own field for it, unambiguous when present
//	enclosure url   the RSS convention for "the bytes are here"
//	guid            only when it is itself a magnet
//
// The guid is last and conditional because it is frequently a permalink to an
// HTML details page, which a download client cannot do anything with. Handing
// one to Add would produce a failure whose message is about the client rather
// than about the indexer.
//
// Returns empty when the indexer offered nothing fetchable. That is a real
// state — some feeds are catalogues — and the grab reports it as the indexer's
// omission rather than as a transfer that went wrong (#225).
//
// Note this is the ONLY place the unhashed guid survives, and it survives into
// a secret.Value. candidateID hashes it for everything else, because the id is
// stored and returned by the API and this is not.
func sourceOf(i item) secret.Value {
	if m, ok := i.attr("magneturl"); ok {
		return secret.Value(strings.TrimSpace(m))
	}
	if u := strings.TrimSpace(i.Enclosure.URL); u != "" {
		return secret.Value(u)
	}
	if g := strings.TrimSpace(i.GUID); strings.HasPrefix(g, "magnet:") {
		return secret.Value(g)
	}
	return ""
}

// digestLength is how much of the hash is kept — 128 bits, which is far past
// collision concerns for the number of releases one search returns and short
// enough to read in a log.
const digestLength = 32

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:digestLength]
}

// ErrUpstream is the indexer, or something in front of it, failing in a way
// that may not still be true in a moment.
//
// Separate from ErrNotTorznab because the two need opposite treatment and
// look identical on the wire: both arrive as a body that is not the protocol.
// One is an address that will never be right, the other is a service that is
// restarting.
var ErrUpstream = errors.New("the indexer failed upstream")

// ErrRateLimited is an indexer asking for room.
//
// Exported because "the indexer is busy" is a normal operational state a
// caller may want to name, and because a job that failed for this reason
// should be retried later rather than reported as broken.
var ErrRateLimited = errors.New("the indexer is rate limiting")

// call performs one request, with retries, and returns the parsed document.
func (c *Client) call(ctx context.Context, params url.Values, attempts int) (any, error) {
	// The credential is added HERE, at the point the request is built, and
	// nowhere else. One line to find when asking where the key goes.
	if c.apiKey != "" {
		params = cloneValues(params)
		params.Set("apikey", c.apiKey)
	}

	target := c.endpoint + "?" + params.Encode()

	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			wait := baseBackoff << (attempt - 1)
			if err := c.sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		doc, err := c.attempt(ctx, target)
		if err == nil {
			return doc, nil
		}
		lastErr = err
		if !retryable(err) {
			// A wrong key does not become right by being asked again, and an
			// endpoint serving JSON does not start serving XML. Retrying
			// either is how a configuration problem turns into a load
			// problem.
			return nil, err
		}
	}
	if attempts == 1 {
		// Not "gave up after 1 attempt", which reads as a client that barely
		// tried. One attempt is the health check's deliberate choice.
		return nil, lastErr
	}
	return nil, fmt.Errorf("gave up after %d attempts: %w", attempts, lastErr)
}

// retryable reports whether trying again could plausibly help.
func retryable(err error) bool {
	if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUpstream) {
		return true
	}
	var perr *ProtocolError
	if errors.As(err, &perr) {
		return !perr.IsConfiguration()
	}
	if errors.Is(err, ErrNotTorznab) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A transport failure — a refused connection, a reset, a timeout at the
	// far end. These are the ones retrying is for.
	return true
}

// attempt performs exactly one request.
func (c *Client) attempt(ctx context.Context, target string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("building the request failed: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL is NOT quoted into this error. It carries the API key, and
		// an error string ends up in logs, in job failure records and in API
		// responses.
		return nil, fmt.Errorf("the indexer could not be reached: %w", scrub(err, c.apiKey))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("reading the indexer's response failed: %w", scrub(err, c.apiKey))
	}

	// 429 is read BEFORE the body, because a rate limiter is entitled to
	// answer with anything at all — an HTML page, nothing — and none of it is
	// a parse failure worth reporting.
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrRateLimited
	}

	// EVERY OTHER STATUS FALLS THROUGH TO THE BODY, including 200 and
	// including 401. See parse() for the measurements that force this.
	doc, perr := parse(resp.StatusCode, body)
	if perr != nil {
		// A 401 or 403 whose body says nothing is STILL a credential problem.
		//
		// Measured: one of the two captured servers rejects a bad key with
		// HTTP 401 and a completely empty body. Without this, that arrives as
		// ErrNotTorznab and an operator is told to check the URL — which is
		// wrong, and wrong in the most expensive direction, because the URL is
		// the one thing that is right.
		//
		// The body is still parsed FIRST: when a server does explain itself
		// its own wording is better than anything inferred from a status.
		if isCredentialStatus(resp.StatusCode) && errors.Is(perr, ErrNotTorznab) {
			return nil, &ProtocolError{
				Code:        errCodeInvalidKey,
				Description: fmt.Sprintf("the endpoint rejected the credential with HTTP %d and said nothing further", resp.StatusCode),
				Status:      resp.StatusCode,
			}
		}
		// A 5xx WITH AN UNREADABLE BODY IS TRANSIENT, AND MUST BE RETRIED.
		//
		// This is the direction in which the status code IS trustworthy, and
		// getting it backwards was a real defect here: a 502 from a reverse
		// proxy in front of the indexer carries an HTML error page or nothing
		// at all, which parses as "not Torznab" — and that was classified as
		// a permanent addressing mistake and never retried. A restarting
		// container would have failed a search outright.
		//
		// The asymmetry is the point of this whole file. A 2xx does not mean
		// success, because the error lives in the body; a 5xx does mean
		// something went wrong upstream, because no Torznab server answers a
		// successful search with one. The body stays authoritative where it
		// says something — a torznab <error> document is returned above,
		// before this — and the status decides only where the body is silent.
		if resp.StatusCode >= http.StatusInternalServerError {
			return nil, fmt.Errorf("%w: HTTP %d, and the body is not Torznab XML (it begins %q)",
				ErrUpstream, resp.StatusCode, excerpt(body))
		}
		return nil, perr
	}
	if resp.StatusCode >= http.StatusBadRequest {
		// A document that parsed, with a failing status and no <error> in it.
		// Rare, and reported honestly rather than treated as success — the
		// alternative is returning an empty feed for an HTTP 500, which is
		// the "no releases found" failure by another route.
		return nil, fmt.Errorf("the indexer answered HTTP %d with a document that "+
			"claims no error", resp.StatusCode)
	}
	return doc, nil
}

// isCredentialStatus reports the statuses that mean "not you" whatever the
// body says.
func isCredentialStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// scrub removes a credential from an error's text.
//
// A transport error quotes the request URL, and the request URL carries the
// API key as a query parameter. This is the one place that can happen, so it
// is handled here rather than trusted not to occur.
func scrub(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	text := err.Error()
	if !strings.Contains(text, secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(text, secret, "REDACTED"))
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v)+1)
	for k, vals := range v {
		out[k] = append([]string(nil), vals...)
	}
	return out
}

// sleepOrDone waits, unless the context gives up first.
func sleepOrDone(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
