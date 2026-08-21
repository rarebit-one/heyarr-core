package indexers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/providers/fixtures"
)

// The replay suite: the recorded corpus, driving the real client.
//
// ADR-0026 — a real indexer proxies real trackers with real credentials and
// can never run in CI. So this is not a stand-in for a live test, it is the
// only test this client will ever have, and the corpus is the only reality it
// will ever see.
//
// Every test here drives the REAL client through httptest. Nothing calls
// parse() directly on a fixture body, because that would prove the parser
// works while leaving the request building, the status handling and the retry
// path untested — which is where both of this milestone's measured traps
// live.

// corpusRoot is the committed corpus, from this package's directory.
func corpusRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "providers", "fixtures", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// capturedServers is the set of real servers in the corpus.
//
// Discovered by READING THE DIRECTORY rather than by a hand-written list.
// The issue is explicit about this: a hand-written list drifts as fixtures
// are added, and the drift is silent — a third server's corpus would be
// committed, exercise nothing, and look exactly like coverage.
func capturedServers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(corpusRoot(t), "torznab"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatal("no torznab corpora at all")
	}
	return out
}

func loadCorpus(t *testing.T, server string) fixtures.Corpus {
	t.Helper()
	c, err := fixtures.Load(corpusRoot(t), "torznab/"+server)
	if err != nil {
		t.Fatalf("loading torznab/%s: %v", server, err)
	}
	return c
}

// clientFor builds a client pointed at a server, with the clock and the sleep
// injected so nothing here waits on wall-clock time.
func clientFor(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := New(Options{
		Name:     "an-indexer",
		Endpoint: endpoint,
		// The corpus records the REDACTED key in its request paths, because
		// redaction happens at capture time. Using it here keeps the fixture
		// self-consistent: the request the client builds matches the request
		// that was recorded.
		APIKey: "REDACTED",
		Now:    func() time.Time { return time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC) },
		Sleep:  func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// serveExchanges answers every request with the named exchange, so a test can
// drive a specific recorded response without depending on the exact query
// string the client happens to build.
func serveExchanges(t *testing.T, c fixtures.Corpus, names ...string) *httptest.Server {
	t.Helper()
	var seq []fixtures.Exchange
	for _, n := range names {
		e, ok := c.Find(n)
		if !ok {
			t.Fatalf("the corpus has no exchange %q; it has %v", n, c.Names())
		}
		seq = append(seq, e)
	}
	var i int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		e := seq[min(i, len(seq)-1)]
		i++
		for k, v := range e.Response.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(e.Response.Status)
		_, _ = w.Write([]byte(e.Response.Body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// Enumeration: every fixture in the corpus is exercised.
// ---------------------------------------------------------------------------

// Every exchange in every torznab corpus must be driven by some test.
//
// Asserted by enumerating the corpus, per the issue: a hand-written list of
// what is covered drifts the moment somebody adds a capture, and a fixture
// nothing drives is a response shape nobody has checked the client against
// while looking exactly like one they have.
func TestEveryFixtureInTheCorpusIsExercised(t *testing.T) {
	for _, server := range capturedServers(t) {
		corpus := loadCorpus(t, server)
		for _, e := range corpus.Exchanges {
			t.Run(server+"/"+e.Name, func(t *testing.T) {
				srv := serveExchanges(t, corpus, e.Name)
				client := clientFor(t, srv.URL)

				// Driven through the client, and what matters is that the
				// outcome is DECIDED — a document or a named error — never a
				// silent empty success.
				_, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"})
				health := client.Check(t.Context())

				switch e.Name {
				case "caps":
					if !health.Healthy {
						t.Errorf("a real caps document was reported unhealthy: %s", health.Detail)
					}
				case "search-with-results", "search-empty":
					// A search fixture cannot satisfy the caps handshake that
					// precedes it, so the error here is about the handshake.
					// What must NOT happen is a healthy report from a feed.
					if health.Healthy {
						t.Errorf("a search feed was accepted as a capabilities document")
					}
				default:
					// Every remaining fixture is a failure shape, and every
					// one of them must be reported as unhealthy rather than
					// read as an empty success.
					if health.Healthy {
						t.Errorf("%q was reported HEALTHY; detail %q", e.Name, health.Detail)
					}
					if err == nil {
						t.Errorf("%q produced no error from Search", e.Name)
					}
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// The trap: an error hidden behind a healthy-looking response.
// ---------------------------------------------------------------------------

// A bad key must never read as "no releases found".
//
// This is the measured trap, and the corpus holds BOTH servers' answers to
// the same mistake:
//
//	Jackett   HTTP 200 with <error code="100" description="Invalid API Key" />
//	Prowlarr  HTTP 401 with an empty body
//
// A client checking only the status code reads the first as a successful
// empty search. A client trusting the status line misses the second. Driving
// both from one table is the point: neither shape may produce a nil error and
// an empty result.
func TestAnInvalidKeyIsNeverReadAsAnEmptySearch(t *testing.T) {
	for _, server := range capturedServers(t) {
		corpus := loadCorpus(t, server)
		if _, ok := corpus.Find("unauthorised"); !ok {
			continue
		}

		// THE SEARCH PATH, with the handshake succeeding first.
		//
		// This sequencing is the whole test and it was wrong to begin with.
		// Serving the rejection to EVERY request — including the caps
		// handshake Search performs first — meant Search failed at the
		// handshake and never reached the response under test. The assertion
		// passed, and it passed for a reason that had nothing to do with the
		// trap: a sabotage that turned a 200 <error> document into an empty
		// feed did not fail it.
		t.Run(server+"/a search", func(t *testing.T) {
			srv := serveExchanges(t, corpus, "caps", "unauthorised")
			client := clientFor(t, srv.URL)

			got, err := client.Search(t.Context(), providers.Query{Title: "ubuntu"})
			if err == nil {
				t.Fatalf("a rejected API key produced NO error and %d candidates — "+
					"this is the failure that reports 'no releases found' forever", len(got))
			}
			if len(got) != 0 {
				t.Errorf("want no candidates alongside the error, got %d", len(got))
			}
			// And it must be named as a credential problem, not as some
			// generic failure: the error decides whether a job retries a
			// wrong key forever.
			var perr *ProtocolError
			if !errors.As(err, &perr) || !perr.IsConfiguration() {
				t.Errorf("a rejected key produced %v, which is not reportable as a "+
					"configuration problem", err)
			}
		})

		// THE HEALTH PATH, where the handshake itself is rejected.
		t.Run(server+"/a health check", func(t *testing.T) {
			srv := serveExchanges(t, corpus, "unauthorised")
			health := clientFor(t, srv.URL).Check(t.Context())

			if health.Healthy {
				t.Fatal("a rejected API key left the provider healthy")
			}
			// Both servers must arrive at the same operator-actionable
			// sentence, though they say completely different things on the
			// wire — 200 with an <error> document, and 401 with no body at
			// all. If this ever reports "check the URL", the URL is the one
			// thing that is right.
			if !strings.Contains(health.Detail, "configuration") {
				t.Errorf("the detail does not tell an operator what to fix: %q", health.Detail)
			}
		})
	}
}

// The two servers really do disagree, and this asserts the disagreement
// rather than describing it in a comment.
//
// If a future recapture makes them agree, this test fails and says so — which
// is the moment to find out, because the whole shape of parse() is justified
// by them differing.
func TestTheTwoServersDisagreeAboutHowToRejectAKey(t *testing.T) {
	statuses := map[string]int{}
	for _, server := range capturedServers(t) {
		corpus := loadCorpus(t, server)
		e, ok := corpus.Find("unauthorised")
		if !ok {
			continue
		}
		if e.Provenance.Origin != fixtures.OriginCaptured {
			continue
		}
		statuses[server] = e.Response.Status
	}
	if len(statuses) < 2 {
		t.Skip("fewer than two captured servers in the corpus")
	}
	seen := map[int]bool{}
	for _, s := range statuses {
		seen[s] = true
	}
	if len(seen) < 2 {
		t.Errorf("both captured servers now reject a key the same way (%v) — the "+
			"status-independent parsing in parse() was justified by them differing, "+
			"so re-read that reasoning before trusting it", statuses)
	}
	if !seen[http.StatusOK] {
		t.Errorf("no captured server rejects a key with HTTP 200 any more (%v); the "+
			"trap this client is built around was measured, and if it is gone the "+
			"design should say so rather than carry it silently", statuses)
	}
}
