package fixtures

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Origin says whether a fixture came from a real service or was written by
// hand.
//
// It exists because the difference is the whole argument of ADR-0026, and a
// corpus that does not record it will be assumed to be real within a week.
type Origin string

const (
	// OriginCaptured is a real response from a real instance. This is what the
	// corpus is supposed to contain.
	OriginCaptured Origin = "captured"
	// OriginSynthesised is written by hand from documentation.
	//
	// Legal, and deliberately uncomfortable to use: it must be justified in
	// the Note, and a test may assert that a given corpus contains none. It
	// exists for shapes a real instance will not readily produce on demand —
	// a 429, say — rather than as a shortcut for not having an instance.
	OriginSynthesised Origin = "synthesised"
)

// Provenance is where a fixture came from.
//
// Every field is required except Note. A capture that cannot say which version
// of which service produced it is one nobody can regenerate, and the first
// time it fails nobody will know whether the client broke or the service
// changed.
type Provenance struct {
	Origin Origin `json:"origin"`
	// Service is "prowlarr", "transmission" — the thing that answered.
	Service string `json:"service"`
	// Version is what that service reported about itself. Not optional: "it
	// worked against some Prowlarr once" is not a fact anyone can act on.
	Version string `json:"version"`
	// CapturedAt is when. A corpus ages, and knowing how much is the
	// difference between "the client is wrong" and "the service moved".
	CapturedAt string `json:"captured_at"`
	// Procedure is how to produce this again — the script and the arguments.
	Procedure string `json:"procedure"`
	// Note carries anything a reader needs, and is REQUIRED for a synthesised
	// fixture: writing one by hand is a decision that has to be justified
	// where the next person meets it.
	Note string `json:"note,omitempty"`
}

// Validate refuses provenance that cannot be acted on.
func (p Provenance) Validate() error {
	switch p.Origin {
	case OriginCaptured, OriginSynthesised:
	case "":
		return errors.New("provenance has no origin — a fixture that does not say " +
			"whether it was captured or invented will be assumed to be real")
	default:
		return fmt.Errorf("%q is not an origin", p.Origin)
	}
	for _, f := range []struct{ name, value string }{
		{"service", p.Service},
		{"version", p.Version},
		{"captured_at", p.CapturedAt},
		{"procedure", p.Procedure},
	} {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("provenance has no %s — a capture nobody can regenerate "+
				"is one nobody can trust the day it starts failing", f.name)
		}
	}
	if p.Origin == OriginSynthesised && strings.TrimSpace(p.Note) == "" {
		return errors.New("a synthesised fixture must justify itself in its note — " +
			"it is the only thing the test will ever see (ADR-0026)")
	}
	if _, err := time.Parse(time.RFC3339, p.CapturedAt); err != nil {
		return fmt.Errorf("captured_at %q is not RFC 3339", p.CapturedAt)
	}
	return nil
}

// Exchange is one recorded request/response pair.
type Exchange struct {
	// Name identifies the case: "search-with-results", "search-empty",
	// "unauthorised", "rate-limited". It is what a test names when it fails.
	Name       string     `json:"name"`
	Provenance Provenance `json:"provenance"`

	// Request is what was sent, enough to match on.
	Request Request `json:"request"`
	// Response is what came back, verbatim apart from redaction.
	Response Response `json:"response"`
}

// Request is the recorded outbound side.
type Request struct {
	Method string `json:"method"`
	// Path includes the query string, since for these APIs the query IS the
	// request.
	Path string `json:"path"`
	Body string `json:"body,omitempty"`
}

// Response is the recorded inbound side.
type Response struct {
	Status int `json:"status"`
	// Headers are only the ones that matter to a client — a content type, a
	// Transmission session id, a Retry-After. Recording every header would
	// make the corpus a diff of the service's deployment rather than of its
	// API.
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body"`
}

// Corpus is a loaded set of exchanges.
type Corpus struct {
	Service   string
	Exchanges []Exchange
}

// Names lists the exchange names, in a stable order.
func (c Corpus) Names() []string {
	out := make([]string, 0, len(c.Exchanges))
	for _, e := range c.Exchanges {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

// Find returns the exchange with the given name.
func (c Corpus) Find(name string) (Exchange, bool) {
	for _, e := range c.Exchanges {
		if e.Name == name {
			return e, true
		}
	}
	return Exchange{}, false
}

// ErrNoCorpus is what an absent corpus produces.
//
// A distinct error because the caller has to be able to tell "this service has
// no captures yet" from "the captures are broken", and those lead to very
// different actions — one is a person with an instance running the capture
// script, the other is a bug.
var ErrNoCorpus = errors.New("fixtures: no corpus for that service")

// Load reads every fixture for a service from a directory.
//
// It REFUSES a fixture with unusable provenance rather than skipping it. A
// corpus that silently drops the files it cannot understand is one that shrinks
// without anybody noticing, and a shrinking corpus still reports every test as
// passing.
func Load(dir, service string) (Corpus, error) {
	root := filepath.Join(dir, service)
	entries, err := os.ReadDir(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Corpus{}, fmt.Errorf("%w: %s", ErrNoCorpus, service)
	case err != nil:
		return Corpus{}, fmt.Errorf("fixtures: reading %s: %w", root, err)
	}

	corpus := Corpus{Service: service}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		raw, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return Corpus{}, fmt.Errorf("fixtures: reading %s: %w", path, err)
		}
		var e Exchange
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&e); err != nil {
			return Corpus{}, fmt.Errorf("fixtures: decoding %s: %w", path, err)
		}
		if e.Name == "" {
			e.Name = strings.TrimSuffix(entry.Name(), ".json")
		}
		if err := e.Provenance.Validate(); err != nil {
			return Corpus{}, fmt.Errorf("fixtures: %s: %w", path, err)
		}
		if e.Provenance.Service != service {
			return Corpus{}, fmt.Errorf("fixtures: %s says it came from %q but lives under %q",
				path, e.Provenance.Service, service)
		}
		corpus.Exchanges = append(corpus.Exchanges, e)
	}
	if len(corpus.Exchanges) == 0 {
		return Corpus{}, fmt.Errorf("%w: %s", ErrNoCorpus, service)
	}
	sort.Slice(corpus.Exchanges, func(i, j int) bool {
		return corpus.Exchanges[i].Name < corpus.Exchanges[j].Name
	})
	return corpus, nil
}

// Server replays a corpus over HTTP, so the REAL client code drives it.
//
// This is the point of the whole package. A replay harness that hands parsed
// values to a test proves the harness works; one that speaks HTTP to the
// client's own transport proves the client parses what the service actually
// sent — which is the only property worth having when the service can never be
// present.
//
// Matching is on method and path. An unmatched request is a 599 with a body
// naming what was asked for and what the corpus holds, because the alternative
// — a 404 — is a status the client may well handle, turning "your test asked
// for something not in the corpus" into "the service said not found".
func (c Corpus) Server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := r.Method + " " + r.URL.RequestURI()
		for _, e := range c.Exchanges {
			if e.Request.Method+" "+e.Request.Path != want {
				continue
			}
			for k, v := range e.Response.Headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(e.Response.Status)
			_, _ = w.Write([]byte(e.Response.Body))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// 599 is not a real status and that is deliberate: no client handles
		// it, so an unmatched request fails loudly instead of being mistaken
		// for a response the service could have given.
		w.WriteHeader(599)
		// The request path is echoed back so a failing test says what it
		// asked for — and it is ESCAPED, because echoing a request into a
		// response body is reflected XSS whatever the content type says. This
		// server only ever answers a test, but a rule that is relaxed "just
		// here" is a rule that is relaxed.
		fmt.Fprintf(w, "no fixture matches %s\navailable:\n", html.EscapeString(want))
		for _, e := range c.Exchanges {
			fmt.Fprintf(w, "  %s %s  (%s)\n",
				html.EscapeString(e.Request.Method),
				html.EscapeString(e.Request.Path),
				html.EscapeString(e.Name))
		}
	}))
}

// ServeOne replays a single exchange, for a test that wants exactly one
// response whatever is requested — a 401 or a rate limit, where the point is
// the status rather than the routing.
func (e Exchange) ServeOne() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range e.Response.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(e.Response.Status)
		_, _ = w.Write([]byte(e.Response.Body))
	}))
}
