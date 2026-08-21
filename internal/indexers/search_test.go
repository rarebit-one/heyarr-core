package indexers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/providers/fixtures"
)

// searchAgainst drives a real search against a corpus, answering the caps
// handshake first and the named feed second.
func searchAgainst(t *testing.T, server, feedName string, q providers.Query) ([]acquisition.ReleaseCandidate, error) {
	t.Helper()
	corpus := loadCorpus(t, server)
	srv := serveExchanges(t, corpus, "caps", feedName)
	return clientFor(t, srv.URL).Search(t.Context(), q)
}

// A real feed from a real server becomes candidates.
func TestARealFeedBecomesCandidates(t *testing.T) {
	for _, server := range []string{"jackett", "prowlarr"} {
		t.Run(server, func(t *testing.T) {
			got, err := searchAgainst(t, server, "search-with-results",
				providers.Query{Title: "ubuntu"})
			if err != nil {
				t.Fatalf("searching a real captured feed failed: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("a feed with items produced no candidates")
			}
			for _, c := range got {
				if strings.TrimSpace(c.Title) == "" {
					t.Error("a candidate has no title")
				}
				if strings.TrimSpace(c.ID) == "" {
					t.Errorf("%q has no id", c.Title)
				}
				if c.Provider != "an-indexer" {
					t.Errorf("%q credits %q rather than the provider that offered it",
						c.Title, c.Provider)
				}
			}
		})
	}
}

// An empty feed is a successful search that found nothing — NOT an error.
//
// Torznab answers a query with no matches with a valid feed containing no
// items, and a client mistaking that for a failure would mark a healthy
// indexer broken every time somebody wanted something obscure. The inverse of
// the invalid-key trap, and it has to be got right in the same breath.
func TestAnEmptyFeedIsASuccessfulSearch(t *testing.T) {
	for _, server := range []string{"jackett", "prowlarr"} {
		t.Run(server, func(t *testing.T) {
			got, err := searchAgainst(t, server, "search-empty",
				providers.Query{Title: "zzzzzzzz-no-such-release"})
			if err != nil {
				t.Fatalf("an empty feed was reported as an error: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("want no candidates, got %d", len(got))
			}
		})
	}
}

// Identity is stable across two parses of the same response.
func TestCandidateIdentityIsStableAcrossParses(t *testing.T) {
	for _, server := range []string{"jackett", "prowlarr"} {
		t.Run(server, func(t *testing.T) {
			first, err := searchAgainst(t, server, "search-with-results",
				providers.Query{Title: "ubuntu"})
			if err != nil {
				t.Fatal(err)
			}
			second, err := searchAgainst(t, server, "search-with-results",
				providers.Query{Title: "ubuntu"})
			if err != nil {
				t.Fatal(err)
			}
			if len(first) != len(second) {
				t.Fatalf("two parses of one response produced %d and %d candidates",
					len(first), len(second))
			}
			for i := range first {
				if first[i].ID != second[i].ID {
					t.Errorf("candidate %d: %q then %q", i, first[i].ID, second[i].ID)
				}
			}
		})
	}
}

// Identity does not depend on where a release appeared in the response.
//
// §63's determinism is only as good as this: an indexer proxies several
// trackers and merges them, so it is free to return the same releases in a
// different order on the next call. A tie broken by position would then select
// a different release for no reason anybody could see — and a stably-ordered
// tie looks exactly like a working system.
func TestCandidateIdentityIsIndependentOfResponseOrder(t *testing.T) {
	for _, server := range []string{"jackett", "prowlarr"} {
		t.Run(server, func(t *testing.T) {
			corpus := loadCorpus(t, server)
			feed, ok := corpus.Find("search-with-results")
			if !ok {
				t.Skip("no feed")
			}
			caps, _ := corpus.Find("caps")

			forward := runFeed(t, caps, feed.Response.Body)
			reversed := runFeed(t, caps, reverseItems(t, feed.Response.Body))

			if len(forward) != len(reversed) {
				t.Fatalf("reversing the response changed the candidate COUNT: %d vs %d",
					len(forward), len(reversed))
			}
			for i := range forward {
				if forward[i].ID != reversed[i].ID {
					t.Fatalf("reversing the response changed candidate %d's identity: "+
						"%q became %q — identity is positional", i, forward[i].ID, reversed[i].ID)
				}
			}
		})
	}
}

// runFeed drives a search against a caps document and a body.
func runFeed(t *testing.T, caps fixtures.Exchange, body string) []acquisition.ReleaseCandidate {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if n.Add(1) == 1 {
			_, _ = w.Write([]byte(caps.Response.Body))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	got, err := clientFor(t, srv.URL).Search(t.Context(), providers.Query{Title: "ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// reverseItems rewrites a feed with its items in the opposite order.
//
// A textual transformation of the real captured body rather than a
// hand-written feed: it keeps every attribute exactly as the server sent it,
// so the only thing that differs is the one thing under test.
func reverseItems(t *testing.T, body string) string {
	t.Helper()
	const open, close = "<item>", "</item>"
	var items []string
	rest := body
	for {
		i := strings.Index(rest, open)
		if i < 0 {
			break
		}
		j := strings.Index(rest[i:], close)
		if j < 0 {
			break
		}
		items = append(items, rest[i:i+j+len(close)])
		rest = rest[i+j+len(close):]
	}
	if len(items) < 2 {
		t.Fatalf("the feed has %d items, so reversing it proves nothing", len(items))
	}
	head := body[:strings.Index(body, open)]
	tail := body[strings.LastIndex(body, close)+len(close):]
	var b strings.Builder
	b.WriteString(head)
	for i := len(items) - 1; i >= 0; i-- {
		b.WriteString(items[i])
	}
	b.WriteString(tail)
	return b.String()
}

// searchSynthesised drives the invented quality-attribute feed behind a REAL
// capabilities handshake.
//
// Deliberately not a synthesised caps document to go with it. The handshake is
// something both real servers produce identically and there is no reason to
// invent one — keeping the fabricated surface to the single document that has
// to be fabricated is what stops a synthesised corpus quietly becoming the
// thing the client is tested against.
func searchSynthesised(t *testing.T) []acquisition.ReleaseCandidate {
	t.Helper()
	caps := mustFind(t, loadCorpus(t, "jackett"), "caps")
	body := mustFind(t, loadCorpus(t, "synthesised"), "search-with-quality-attributes")
	return runFeed(t, caps, body.Response.Body)
}

// ---------------------------------------------------------------------------
// Attributes
// ---------------------------------------------------------------------------

// Against the two REAL servers, size is determined and nothing else is.
//
// Stated as an assertion rather than left in a comment, because it is the
// honest coverage claim of this whole change and it should fail loudly if a
// recapture ever changes it.
func TestAgainstRealServersOnlySizeIsDetermined(t *testing.T) {
	for _, server := range []string{"jackett", "prowlarr"} {
		t.Run(server, func(t *testing.T) {
			got, err := searchAgainst(t, server, "search-with-results",
				providers.Query{Title: "ubuntu"})
			if err != nil {
				t.Fatal(err)
			}
			var withSize int
			for _, c := range got {
				for attr := range c.Attributes {
					if attr != policy.AttrSizeBytes {
						t.Errorf("%q determined %q from a real feed — no real capture "+
							"carries a quality attribute, so this came from somewhere "+
							"it should not have", c.Title, attr)
					}
				}
				if _, ok := c.Attributes[policy.AttrSizeBytes]; ok {
					withSize++
				}
			}
			if withSize == 0 {
				t.Error("no candidate had a size, though every captured item has a <size>")
			}
		})
	}
}

// An attribute that is present-but-empty counts as ABSENT.
//
// Measured: one of the two servers emits name="genre" value="" on every item.
// Treating that as a determined empty string would hand §63 a confident answer
// nobody has.
func TestAnEmptyAttributeCountsAsAbsent(t *testing.T) {
	got := searchSynthesised(t)
	var checked bool
	for _, c := range got {
		if !strings.Contains(c.Title, "2 0") {
			continue
		}
		checked = true
		// That item carries language="" and video="   ".
		if v, ok := c.Attributes[policy.AttrLanguage]; ok {
			t.Errorf("an empty attribute was determined as %v", v)
		}
		if v, ok := c.Attributes[policy.AttrVideoCodec]; ok {
			t.Errorf("a whitespace-only attribute was determined as %v", v)
		}
		// ... and a resolution that IS readable, so this is not passing
		// because nothing was extracted at all.
		if v, ok := c.Attributes[policy.AttrResolution]; !ok || v.Num != 2160 {
			t.Errorf("want resolution 2160 on the same item, got %v (present=%v)", v, ok)
		}
	}
	if !checked {
		t.Fatal("the item this test is about was not in the result")
	}
}

// A value that cannot be read is left out rather than coerced.
func TestAnUnreadableAttributeIsLeftOut(t *testing.T) {
	got := searchSynthesised(t)
	var checked bool
	for _, c := range got {
		if !strings.Contains(c.Title, "3 0") {
			continue
		}
		checked = true
		// resolution="hd" is not a number and must not become one — least of
		// all zero, which would read as a determined resolution of 0.
		if v, ok := c.Attributes[policy.AttrResolution]; ok {
			t.Errorf(`resolution "hd" was coerced to %v`, v)
		}
		if v, ok := c.Attributes[policy.AttrAudioCodec]; !ok || v.Text != "flac" {
			t.Errorf("want the readable attribute on the same item, got %v (present=%v)", v, ok)
		}
	}
	if !checked {
		t.Fatal("the item this test is about was not in the result")
	}
}

// Both spellings of a resolution mean the same number.
func TestBothSpellingsOfAResolutionAreOneNumber(t *testing.T) {
	got := searchSynthesised(t)
	want := map[string]int64{"1 0": 1080, "2 0": 2160}
	seen := map[string]bool{}
	for _, c := range got {
		for marker, n := range want {
			if !strings.Contains(c.Title, marker) {
				continue
			}
			seen[marker] = true
			if v, ok := c.Attributes[policy.AttrResolution]; !ok || v.Num != n {
				t.Errorf("%q: want resolution %d, got %v (present=%v)", c.Title, n, v, ok)
			}
		}
	}
	for marker := range want {
		if !seen[marker] {
			t.Errorf("the item %q was not in the result", marker)
		}
	}
}

// Nothing is derived from a release title.
//
// The corpus is full of titles that a regular expression would happily mine —
// "amd64", version numbers, "iso". If any attribute appears that the document
// did not assert, something has started reading the title.
func TestNothingIsExtractedFromATitle(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel><item>
    <title>something 2160p bluray hevc truehd atmos english HDR10 x265</title>
    <guid>ffffffffffffffffffffffffffffffffffffffff</guid>
  </item></channel></rss>`

	caps := mustFind(t, loadCorpus(t, "jackett"), "caps")
	got := runFeed(t, caps, body)
	if len(got) != 1 {
		t.Fatalf("want one candidate, got %d", len(got))
	}
	if len(got[0].Attributes) != 0 {
		t.Fatalf("a title alone produced attributes %v — a title is a filename "+
			"written by a stranger, not evidence", got[0].Attributes)
	}
}

// ---------------------------------------------------------------------------
// §63: an absent attribute is reported as undetermined, with a reason.
// ---------------------------------------------------------------------------

// The end of the chain the issue names: a real candidate, a real profile, and
// an evaluation that says "could not determine" rather than "failed".
func TestAnAbsentAttributeEvaluatesAsUndetermined(t *testing.T) {
	got, err := searchAgainst(t, "jackett", "search-with-results",
		providers.Query{Title: "ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no candidates")
	}

	profile := policy.Profile{
		Name: "wants-1080p",
		Accept: []policy.Rule{{
			Attribute: policy.AttrResolution,
			Op:        policy.OpGTE,
			Value:     policy.Num(1080),
		}},
	}
	ev := acquisition.Evaluate(got[0], profile)

	var found bool
	for _, r := range ev.Reasons {
		if !strings.Contains(r.Rule, string(policy.AttrResolution)) {
			continue
		}
		found = true
		if r.Result != acquisition.ResultUndetermined {
			t.Errorf("a resolution nobody could determine evaluated as %q, not %q — "+
				"that is a confident wrong answer with no reason attached",
				r.Result, acquisition.ResultUndetermined)
		}
	}
	if !found {
		t.Fatalf("no reason mentioned the resolution rule; reasons were %+v", ev.Reasons)
	}
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

// A 429 backs off and eventually succeeds, rather than propagating.
//
// An indexer returning 429 is normal operation, not an error to hand upwards:
// it proxies trackers that impose their own limits, and pressing one that has
// asked for room is how an operator's account gets banned from a tracker they
// care about.
func TestARateLimitBacksOffAndThenSucceeds(t *testing.T) {
	corpus := loadCorpus(t, "jackett")
	capsBody := mustFind(t, corpus, "caps").Response.Body
	feedBody := mustFind(t, corpus, "search-empty").Response.Body

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		// The caps handshake succeeds; the SEARCH is rate limited twice and
		// then answered.
		if r.URL.Query().Get("t") == "caps" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(capsBody))
			return
		}
		if n <= 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(feedBody))
	}))
	t.Cleanup(srv.Close)

	var waits []time.Duration
	client, err := New(Options{
		Name:     "an-indexer",
		Endpoint: srv.URL,
		APIKey:   "REDACTED",
		Sleep: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err != nil {
		t.Fatalf("a rate limit that cleared was propagated as a failure: %v", err)
	}
	if len(waits) == 0 {
		t.Fatal("it retried without waiting at all, which is pressing an indexer " +
			"that asked for room")
	}
	for i := 1; i < len(waits); i++ {
		if waits[i] <= waits[i-1] {
			t.Errorf("backoff did not grow: %v", waits)
			break
		}
	}
}

// A rate limit that never clears is an honest failure, not an infinite loop.
func TestARateLimitThatNeverClearsGivesUp(t *testing.T) {
	capsBody := mustFind(t, loadCorpus(t, "jackett"), "caps").Response.Body
	var searches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") == "caps" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(capsBody))
			return
		}
		searches.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	client, err := New(Options{
		Name: "an-indexer", Endpoint: srv.URL, APIKey: "REDACTED",
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err == nil {
		t.Fatal("a rate limit that never cleared reported success")
	}
	if got := searches.Load(); got != maxAttempts {
		t.Errorf("want exactly %d attempts, got %d", maxAttempts, got)
	}
}

// A wrong key is NOT retried. It will not become right.
func TestAConfigurationErrorIsNotRetried(t *testing.T) {
	corpus := loadCorpus(t, "jackett")
	bad := mustFind(t, corpus, "unauthorised").Response.Body

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(bad))
	}))
	t.Cleanup(srv.Close)

	client := clientFor(t, srv.URL)
	if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err == nil {
		t.Fatal("a rejected key reported success")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("a rejected API key was retried %d times; it will not become right", got)
	}
}

// ---------------------------------------------------------------------------
// The request the client builds
// ---------------------------------------------------------------------------

func TestTheRequestCarriesTheProtocolsParameters(t *testing.T) {
	capsBody := mustFind(t, loadCorpus(t, "jackett"), "caps").Response.Body
	feedBody := mustFind(t, loadCorpus(t, "jackett"), "search-empty").Response.Body

	var searchQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("t") == "caps" {
			_, _ = w.Write([]byte(capsBody))
			return
		}
		searchQuery = r.URL.Query()
		_, _ = w.Write([]byte(feedBody))
	}))
	t.Cleanup(srv.Close)

	client := clientFor(t, srv.URL)
	if _, err := client.Search(t.Context(), providers.Query{
		Title: "ubuntu", Year: 2024, ContentType: "movie", Limit: 25,
	}); err != nil {
		t.Fatal(err)
	}

	// The captured caps document advertises search=yes and movie-search=no,
	// so a movie query must fall back to the general search rather than
	// asking for a function the indexer said it does not have.
	if got := searchQuery.Get("t"); got != "search" {
		t.Errorf("want the general search, got t=%q — the indexer advertised "+
			"movie-search=no", got)
	}
	if got := searchQuery.Get("apikey"); got != "REDACTED" {
		t.Errorf("the credential did not reach the request: apikey=%q", got)
	}
	if got := searchQuery.Get("limit"); got != "25" {
		t.Errorf("want limit=25, got %q", got)
	}
	if got := searchQuery.Get("q"); !strings.Contains(got, "ubuntu") ||
		!strings.Contains(got, "2024") {
		t.Errorf("want the title and the year in q, got %q", got)
	}
}

// An indexer that cannot search at all is REPORTED as such, not queried and
// found wanting.
func TestAnIndexerThatAdvertisesNoSearchIsRefused(t *testing.T) {
	capsBody := `<?xml version="1.0" encoding="UTF-8"?>
<caps><server title="Something" /><searching>
  <search available="no" /><tv-search available="no" /><movie-search available="no" />
</searching></caps>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(capsBody))
	}))
	t.Cleanup(srv.Close)

	_, err := clientFor(t, srv.URL).Search(t.Context(), providers.Query{Title: "ubuntu"})
	if err == nil {
		t.Fatal("an indexer advertising no search at all was queried anyway")
	}
	if !strings.Contains(err.Error(), "cannot search") {
		t.Errorf("the error does not say the indexer cannot search: %v", err)
	}
}

// The credential never appears in an error, however the transport fails.
func TestACredentialNeverReachesAnError(t *testing.T) {
	const secret = "a-very-secret-key-0123456789"
	// A server that is closed immediately, so Do() fails with a URL-quoting
	// transport error — the one place a key can escape.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := srv.URL
	srv.Close()

	client, err := New(Options{
		Name: "an-indexer", Endpoint: endpoint, APIKey: secret,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, searchErr := client.Search(t.Context(), providers.Query{Title: "ubuntu"})
	if searchErr == nil {
		t.Fatal("a closed endpoint reported success")
	}
	if strings.Contains(searchErr.Error(), secret) {
		t.Fatalf("the API key is in the error text, which reaches logs and the "+
			"API: %v", searchErr)
	}
	health := client.Check(t.Context())
	if strings.Contains(health.Detail, secret) {
		t.Fatalf("the API key is in the health detail, which is served by "+
			"GET /api/v1/providers: %s", health.Detail)
	}
}

// A malformed or non-XML body names what failed rather than reporting
// "no releases found".
func TestANonTorznabBodyNamesWhatArrived(t *testing.T) {
	for _, server := range []string{"jackett", "prowlarr"} {
		t.Run(server, func(t *testing.T) {
			corpus := loadCorpus(t, server)
			e := mustFind(t, corpus, "indexer-not-found")
			srv := serveExchanges(t, corpus, "indexer-not-found")

			_, err := clientFor(t, srv.URL).Search(t.Context(), providers.Query{Title: "ubuntu"})
			if err == nil {
				t.Fatalf("a %d with a JSON body was read as a successful search",
					e.Response.Status)
			}
			if !strings.Contains(err.Error(), "Torznab") {
				t.Errorf("the error does not say the endpoint is not serving "+
					"Torznab: %v", err)
			}
		})
	}
}

func mustFind(t *testing.T, c fixtures.Corpus, name string) fixtures.Exchange {
	t.Helper()
	e, ok := c.Find(name)
	if !ok {
		t.Fatalf("the corpus has no %q; it has %v", name, c.Names())
	}
	return e
}

// A health check makes exactly ONE attempt.
//
// The health job runs across every configured provider, and an unreachable
// one is the common case rather than the exception. Retrying inside a check
// would hold the job's lease through a full backoff per provider, and would
// smooth over the flapping the endpoint exists to reveal.
func TestAHealthCheckDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		// A transport-shaped failure, which IS retryable — so a client that
		// retries in Check will show it here.
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	client := clientFor(t, srv.URL)
	if h := client.Check(t.Context()); h.Healthy {
		t.Fatal("a 502 was reported healthy")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("a health check made %d attempts; one is the observation", got)
	}
}

// A search, by contrast, does persist.
func TestASearchRetriesWhereAHealthCheckDoesNot(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	client, err := New(Options{
		Name: "an-indexer", Endpoint: srv.URL, APIKey: "REDACTED",
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"}); err == nil {
		t.Fatal("a persistently failing endpoint reported success")
	}
	if got := calls.Load(); got != maxAttempts {
		t.Errorf("a search made %d attempts, want %d", got, maxAttempts)
	}
}
