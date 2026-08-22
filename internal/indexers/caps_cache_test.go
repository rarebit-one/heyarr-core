package indexers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The capabilities cache (#131).
//
// Every assertion here is about REQUEST COUNTS against an httptest server,
// never about elapsed time. A cache that "works" while still handshaking is
// exactly the failure this suite exists to catch, and it is invisible to any
// test that only checks the answer came back right.
//
// ADR-0026: fixtures and httptest only. Nothing in this file opens a socket to
// anything but a test server, and the capabilities documents are the REAL
// captured ones — including the reconfigured indexer, which is the captured
// document with one attribute flipped rather than an invention. Flipping one
// attribute of a real capture keeps every other thing the server said exactly
// as it said it, so the only difference is the thing under test.

// ---------------------------------------------------------------------------
// A test indexer that counts what it was asked
// ---------------------------------------------------------------------------

// capsProbe is an httptest server that answers the two Torznab requests
// separately and counts each.
//
// The counts are separate on purpose: "how many searches happened" and "how
// many handshakes happened" are the two numbers this whole issue is about, and
// a single total would let a saved handshake hide behind an extra search.
type capsProbe struct {
	server *httptest.Server

	handshakes atomic.Int32
	searches   atomic.Int32

	mu sync.Mutex
	// caps is what t=caps answers with; capsStatus is the status it answers
	// with. Both swappable mid-test, which is how an indexer that is
	// reconfigured, and one that goes away, are reproduced.
	caps       string
	capsStatus int
	// feed is what every other function answers with.
	feed string
	// lastFunction is the t= parameter of the most recent non-caps request,
	// so a test can assert WHICH search the cached document chose.
	lastFunction string
}

func newCapsProbe(t *testing.T, capsBody, feedBody string) *capsProbe {
	t.Helper()
	p := &capsProbe{caps: capsBody, capsStatus: http.StatusOK, feed: feedBody}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("t") == "caps" {
			p.handshakes.Add(1)
			w.WriteHeader(p.capsStatus)
			_, _ = w.Write([]byte(p.caps))
			return
		}
		p.searches.Add(1)
		p.lastFunction = r.URL.Query().Get("t")
		_, _ = w.Write([]byte(p.feed))
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *capsProbe) serveCaps(body string, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.caps, p.capsStatus = body, status
}

func (p *capsProbe) function() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastFunction
}

// testClock is the injected clock (CLAUDE.md: an injected clock, never
// time.Sleep — a TTL test that slept would take ten minutes and would still be
// asserting on the wall clock rather than on the rule).
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *testClock {
	return &testClock{at: time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// cachingClientFor is clientFor with a clock a test can move.
func cachingClientFor(t *testing.T, endpoint string, clock *testClock) *Client {
	t.Helper()
	c, err := New(Options{
		Name:     "an-indexer",
		Endpoint: endpoint,
		APIKey:   "REDACTED",
		Now:      clock.now,
		Sleep:    func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// realCaps is a captured capabilities document.
func realCaps(t *testing.T, server string) string {
	t.Helper()
	return mustFind(t, loadCorpus(t, server), "caps").Response.Body
}

// realFeed is a captured search result.
func realFeed(t *testing.T, server string) string {
	t.Helper()
	return mustFind(t, loadCorpus(t, server), "search-with-results").Response.Body
}

// reconfiguredForMovies is a captured caps document with movie search turned
// ON, as an operator ticking a box in their indexer's web UI would produce.
//
// A textual flip of the real capture rather than a hand-written document, for
// the same reason reverseItems is: everything the server actually said stays
// exactly as it said it, and the only difference is the one under test.
func reconfiguredForMovies(t *testing.T, body string) string {
	t.Helper()
	const was = `<movie-search available="no"`
	const now = `<movie-search available="yes"`
	if !strings.Contains(body, was) {
		t.Fatalf("the captured caps document no longer advertises movie-search=no, so "+
			"flipping it proves nothing; it says %q", excerpt([]byte(body)))
	}
	return strings.Replace(body, was, now, 1)
}

// ---------------------------------------------------------------------------
// The saving itself
// ---------------------------------------------------------------------------

// A second search inside the TTL costs ONE request, not two.
//
// The whole issue, as a number. Asserted on the request count rather than on
// anything time-shaped, because a cache that answers correctly while still
// handshaking looks identical from the outside in every other respect.
func TestASecondSearchWithinTheTTLDoesNotHandshakeAgain(t *testing.T) {
	for _, server := range []string{"jackett", "prowlarr"} {
		t.Run(server, func(t *testing.T) {
			clock := newClock()
			probe := newCapsProbe(t, realCaps(t, server), realFeed(t, server))
			client := cachingClientFor(t, probe.server.URL, clock)

			if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err != nil {
				t.Fatal(err)
			}
			if got := probe.handshakes.Load(); got != 1 {
				t.Fatalf("the first search made %d handshakes, want 1", got)
			}

			// Well inside the TTL, and moving the clock at all proves the
			// entry is trusted for a duration rather than for one call.
			clock.advance(capsTTL / 2)
			if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err != nil {
				t.Fatal(err)
			}

			if got := probe.handshakes.Load(); got != 1 {
				t.Errorf("two searches made %d handshakes; the second one is the "+
					"round trip this issue is about", got)
			}
			if got := probe.searches.Load(); got != 2 {
				t.Errorf("two searches made %d search requests, want 2 — the cache "+
					"must save the HANDSHAKE, not the search", got)
			}
		})
	}
}

// A search after the TTL handshakes again.
//
// The half of the cache that matters more than the saving: an indexer that has
// been reconfigured must not stay "incapable" until somebody restarts Heyarr.
func TestASearchAfterTheTTLHandshakesAgain(t *testing.T) {
	clock := newClock()
	probe := newCapsProbe(t, realCaps(t, "jackett"), realFeed(t, "jackett"))
	client := cachingClientFor(t, probe.server.URL, clock)

	if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err != nil {
		t.Fatal(err)
	}

	// One nanosecond short of the TTL is still fresh. Asserted so that the
	// expiry below is known to be the TTL and not merely "some time later".
	clock.advance(capsTTL - time.Nanosecond)
	if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err != nil {
		t.Fatal(err)
	}
	if got := probe.handshakes.Load(); got != 1 {
		t.Fatalf("an entry one nanosecond short of the TTL was re-fetched: %d handshakes", got)
	}

	clock.advance(time.Nanosecond)
	if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err != nil {
		t.Fatal(err)
	}
	if got := probe.handshakes.Load(); got != 2 {
		t.Errorf("a search after the TTL made %d handshakes in total, want 2 — a "+
			"cache that never expires is how a reconfigured indexer stays "+
			"'incapable' until a restart", got)
	}
}

// ---------------------------------------------------------------------------
// Invalidation: the health check refreshes what is cached
// ---------------------------------------------------------------------------

// A health check replaces the cached capabilities with what it just observed.
//
// Asserted in three steps, and the middle one is the one that gives the test
// its teeth: the cached document is shown to be STALE — the indexer now
// advertises movie search and Heyarr is still choosing the general search —
// before the check runs and the same query starts choosing t=movie.
//
// Without the staleness step, a client that had simply re-handshaked on its
// own would pass.
func TestAHealthCheckRefreshesTheCachedCapabilities(t *testing.T) {
	clock := newClock()
	original := realCaps(t, "jackett")
	probe := newCapsProbe(t, original, realFeed(t, "jackett"))
	client := cachingClientFor(t, probe.server.URL, clock)

	movies := providers.Query{Title: "ubuntu", ContentType: "movie"}

	// 1. The cache is primed from an indexer that says movie-search=no, so a
	//    movie query falls back to the general search.
	if _, err := client.Search(t.Context(), movies); err != nil {
		t.Fatal(err)
	}
	if got := probe.function(); got != "search" {
		t.Fatalf("want the general search from the captured document, got t=%q", got)
	}

	// 2. The operator enables movie search. The cache does not know, and this
	//    is asserted rather than assumed — it is what makes step 3 mean
	//    something.
	probe.serveCaps(reconfiguredForMovies(t, original), http.StatusOK)
	before := probe.handshakes.Load()
	if _, err := client.Search(t.Context(), movies); err != nil {
		t.Fatal(err)
	}
	if got := probe.handshakes.Load(); got != before {
		t.Fatalf("the cached entry was re-fetched without being asked to: %d handshakes", got)
	}
	if got := probe.function(); got != "search" {
		t.Fatalf("the cached document was not stale after all — it already chose t=%q, "+
			"so this test cannot prove the check refreshed anything", got)
	}

	// 3. The health check exercises t=caps anyway (#99). Having it write the
	//    cache is what keeps health and search from ever disagreeing, and is
	//    the operator's force-refresh path.
	if h := client.Check(t.Context()); !h.Healthy {
		t.Fatalf("the reconfigured indexer was reported unhealthy: %s", h.Detail)
	}

	after := probe.handshakes.Load()
	if _, err := client.Search(t.Context(), movies); err != nil {
		t.Fatal(err)
	}
	if got := probe.handshakes.Load(); got != after {
		t.Errorf("the search handshaked again after the check had just done it "+
			"(%d then %d) — the check's observation was thrown away", after, got)
	}
	if got := probe.function(); got != "movie" {
		t.Errorf("after a health check the cached capabilities still choose t=%q; "+
			"the check refreshed nothing, so an operator who enabled movie search "+
			"has no way to make Heyarr notice", got)
	}
}

// A failed health check does not throw away a good entry.
//
// The failure is the case a stale entry exists for; evicting on it would leave
// the next search with nothing at exactly the moment it needs something.
func TestAFailedHealthCheckDoesNotDiscardTheCachedCapabilities(t *testing.T) {
	clock := newClock()
	probe := newCapsProbe(t, realCaps(t, "jackett"), realFeed(t, "jackett"))
	client := cachingClientFor(t, probe.server.URL, clock)

	if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err != nil {
		t.Fatal(err)
	}

	probe.serveCaps("", http.StatusBadGateway)
	if h := client.Check(t.Context()); h.Healthy {
		t.Fatal("a 502 handshake was reported healthy")
	}

	// Still inside the TTL, so this must not even ask.
	before := probe.handshakes.Load()
	if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err != nil {
		t.Fatalf("a failed health check left the client with no capabilities: %v", err)
	}
	if got := probe.handshakes.Load(); got != before {
		t.Errorf("a failed check evicted a still-fresh entry: %d then %d handshakes",
			before, got)
	}
}

// ---------------------------------------------------------------------------
// Decision 4: a stale entry during a blip, and never over a configuration error
// ---------------------------------------------------------------------------

// An indexer that cannot answer the handshake right now is served from the
// stale entry rather than refused.
//
// A restarting indexer, or one behind a proxy that answered 502, has not been
// reconfigured — its capabilities are exactly what they were. Refusing the
// search there would turn a momentary handshake failure into a failed search
// when the search itself would have succeeded.
func TestAnUnreachableIndexerIsServedFromTheStaleEntry(t *testing.T) {
	clock := newClock()
	probe := newCapsProbe(t, realCaps(t, "jackett"), realFeed(t, "jackett"))
	client := cachingClientFor(t, probe.server.URL, clock)

	if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err != nil {
		t.Fatal(err)
	}

	// The entry expires AND the handshake starts failing. The search endpoint
	// still answers, which is the blip this decision is about: the indexer is
	// there, it just did not manage the handshake this second.
	clock.advance(capsTTL + time.Second)
	probe.serveCaps("", http.StatusBadGateway)

	before := probe.handshakes.Load()
	got, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"})
	if err != nil {
		t.Fatalf("a blip in the handshake failed a search the indexer could have "+
			"answered: %v", err)
	}
	if len(got) == 0 {
		t.Error("the stale entry produced no candidates from a feed that has items")
	}
	// And it DID try to refresh first. Serving stale without asking would be a
	// cache that never expires wearing a different name.
	if now := probe.handshakes.Load(); now <= before {
		t.Errorf("the expired entry was served without attempting a refresh "+
			"(%d then %d handshakes)", before, now)
	}
}

// A rejected credential is NOT papered over by a stale entry.
//
// The other half of decision 4, and the half that keeps it honest. An API key
// that has been rotated will not become right on the next attempt, so
// proceeding on a remembered document would spend a second request to reach
// the same refusal with a worse explanation.
func TestAConfigurationErrorIsNotPaperedOverByAStaleEntry(t *testing.T) {
	clock := newClock()
	probe := newCapsProbe(t, realCaps(t, "jackett"), realFeed(t, "jackett"))
	client := cachingClientFor(t, probe.server.URL, clock)

	if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err != nil {
		t.Fatal(err)
	}

	rejection := mustFind(t, loadCorpus(t, "jackett"), "unauthorised")
	clock.advance(capsTTL + time.Second)
	probe.serveCaps(rejection.Response.Body, rejection.Response.Status)

	searchesBefore := probe.searches.Load()
	_, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"})
	if err == nil {
		t.Fatal("a rejected API key was hidden behind a stale capabilities entry, " +
			"which is the 'no releases found forever' failure by another route")
	}
	var perr *ProtocolError
	if !errors.As(err, &perr) || !perr.IsConfiguration() {
		t.Errorf("want a configuration-shaped protocol error, got %v", err)
	}
	if got := probe.searches.Load(); got != searchesBefore {
		t.Errorf("the search was sent anyway after the handshake was rejected "+
			"(%d then %d)", searchesBefore, got)
	}
}

// ---------------------------------------------------------------------------
// ADR-0028: the refusal must survive the cache. This is the one that must not
// regress.
// ---------------------------------------------------------------------------

// An indexer that says it cannot search is refused from the cache exactly as
// it is from the wire.
//
// The live refusal is reproduced FIRST and its error captured, and the cached
// path is then required to produce the same one — with no request of any kind
// behind it. Caching the answer must never become caching the DECISION away:
// the difference between "this indexer does not do music" and "no music was
// found" looks identical in a result set and leads to completely different
// actions, and it is the reason the handshake exists at all.
func TestACachedRefusalIsIdenticalToALiveOne(t *testing.T) {
	// A captured document with every search turned off — the same textual
	// flip as reconfiguredForMovies, in the other direction.
	body := strings.ReplaceAll(realCaps(t, "jackett"), `<search available="yes"`, `<search available="no"`)
	if strings.Contains(body, `available="yes"`) {
		t.Fatalf("the captured document still advertises something searchable, so "+
			"it cannot stand in for an indexer that refuses: %q", excerpt([]byte(body)))
	}

	clock := newClock()
	probe := newCapsProbe(t, body, realFeed(t, "jackett"))
	client := cachingClientFor(t, probe.server.URL, clock)

	// The LIVE refusal, from a handshake performed on the wire.
	_, live := client.Search(t.Context(), providers.Query{Title: "ubuntu", ContentType: "music"})
	if live == nil {
		t.Fatal("an indexer advertising no search at all was queried anyway")
	}
	if !errors.Is(live, ErrUnsupportedContentType) {
		t.Fatalf("the live refusal is not an unsupported-content-type error: %v", live)
	}
	if got := probe.searches.Load(); got != 0 {
		t.Fatalf("the refusal still sent %d searches; it is supposed to be a refusal", got)
	}

	// The CACHED refusal. Same query, same client, nothing asked of the
	// indexer at all.
	handshakes := probe.handshakes.Load()
	_, cached := client.Search(t.Context(), providers.Query{Title: "ubuntu", ContentType: "music"})
	if cached == nil {
		t.Fatal("the cached capabilities let a search through that the live ones " +
			"refused — a cache hit skipped the unsupported-function check, which " +
			"turns an honest refusal into a silent empty result")
	}
	if !errors.Is(cached, ErrUnsupportedContentType) {
		t.Errorf("the cached refusal is not an unsupported-content-type error: %v", cached)
	}
	if cached.Error() != live.Error() {
		t.Errorf("the cached refusal reads differently from the live one:\n live:   %v\n cached: %v",
			live, cached)
	}
	if got := probe.handshakes.Load(); got != handshakes {
		t.Errorf("the cached refusal handshaked again (%d then %d)", handshakes, got)
	}
	if got := probe.searches.Load(); got != 0 {
		t.Errorf("the cached path sent %d searches to an indexer that said it cannot "+
			"search", got)
	}
}
