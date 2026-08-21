package fixtures

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validProvenance() Provenance {
	return Provenance{
		Origin:     OriginCaptured,
		Service:    "torznab",
		Server:     "Jackett",
		Version:    "1.21.2",
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		Procedure:  "scripts/capture-fixtures.sh torznab <endpoint> <key>",
	}
}

// A synthesised fixture has no server, and demanding one would invite a
// plausible-looking lie in the field that exists to prevent exactly that.
func TestASynthesisedFixtureNeedsNoServer(t *testing.T) {
	p := validProvenance()
	p.Origin = OriginSynthesised
	p.Server = ""
	p.Note = "written by hand; the shape is from the specification"
	if err := p.Validate(); err != nil {
		t.Fatalf("a synthesised fixture was required to name a server: %v", err)
	}
}

// Provenance is not decoration to be filled in later. An unlabelled fixture
// becomes a trusted one within a week, and the whole argument of ADR-0026 is
// the difference between captured and invented.
func TestProvenanceRefusesWhatCannotBeActedOn(t *testing.T) {
	cases := []struct {
		name string
		with func(*Provenance)
		want string
	}{
		{"no origin", func(p *Provenance) { p.Origin = "" }, "does not say whether"},
		{"an unknown origin", func(p *Provenance) { p.Origin = "borrowed" }, "not an origin"},
		{"no service", func(p *Provenance) { p.Service = "" }, "no service"},
		{
			// Required for a CAPTURE only. The corpus holds two servers
			// speaking one protocol, and its central claim — that this client
			// is bound to the protocol rather than shaped to one product — is
			// unreadable if a fixture cannot say which product answered.
			"a capture that does not say which server answered",
			func(p *Provenance) { p.Server = "" },
			"no server",
		},
		{"no version", func(p *Provenance) { p.Version = "" }, "no version"},
		{"no capture time", func(p *Provenance) { p.CapturedAt = "" }, "no captured_at"},
		{"no procedure", func(p *Provenance) { p.Procedure = "" }, "no procedure"},
		{"a capture time that is not a time", func(p *Provenance) { p.CapturedAt = "last Tuesday" }, "RFC 3339"},
		{
			// Writing one by hand is a decision, and it has to be justified
			// where the next person meets it.
			"a synthesised fixture with no justification",
			func(p *Provenance) { p.Origin = OriginSynthesised; p.Note = "" },
			"must justify itself",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validProvenance()
			tc.with(&p)
			err := p.Validate()
			if err == nil {
				t.Fatal("this provenance should be refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal should explain %q, said: %v", tc.want, err)
			}
		})
	}

	if err := validProvenance().Validate(); err != nil {
		t.Errorf("a complete provenance should validate: %v", err)
	}
	p := validProvenance()
	p.Origin = OriginSynthesised
	p.Note = "a 429 could not be provoked from a healthy instance"
	if err := p.Validate(); err != nil {
		t.Errorf("a justified synthesised fixture is legal: %v", err)
	}
}

// A corpus that silently drops the files it cannot understand is one that
// shrinks without anybody noticing — and a shrinking corpus still reports
// every test as passing.
func TestLoadRefusesRatherThanSkips(t *testing.T) {
	dir := t.TempDir()
	service := filepath.Join(dir, "torznab")
	if err := os.MkdirAll(service, 0o750); err != nil {
		t.Fatal(err)
	}
	writeExchange(t, filepath.Join(service, "good.json"), Exchange{
		Name: "good", Provenance: validProvenance(),
		Request:  Request{Method: http.MethodGet, Path: "/api/v1/search?query=x"},
		Response: Response{Status: 200, Body: `[]`},
	})
	// One with no provenance at all.
	if err := os.WriteFile(filepath.Join(service, "bad.json"),
		[]byte(`{"name":"bad","request":{"method":"GET","path":"/x"},"response":{"status":200,"body":""}}`),
		0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(dir, "torznab"); err == nil {
		t.Fatal("a fixture with unusable provenance must fail the load, not be skipped")
	}
}

// A fixture filed under the wrong service is a corpus that lies about where its
// contents came from.
func TestLoadRefusesAMisfiledFixture(t *testing.T) {
	dir := t.TempDir()
	service := filepath.Join(dir, "transmission")
	if err := os.MkdirAll(service, 0o750); err != nil {
		t.Fatal(err)
	}
	p := validProvenance() // says torznab
	writeExchange(t, filepath.Join(service, "x.json"), Exchange{
		Name: "x", Provenance: p,
		Request:  Request{Method: http.MethodPost, Path: "/transmission/rpc"},
		Response: Response{Status: 200, Body: `{}`},
	})
	_, err := Load(dir, "transmission")
	if err == nil || !strings.Contains(err.Error(), "says it came from") {
		t.Fatalf("expected a misfiling refusal, got %v", err)
	}
}

// "No captures yet" and "the captures are broken" lead to very different
// actions — one is a person with an instance running a script, the other is a
// bug — so they must be distinguishable.
func TestAnAbsentCorpusIsTyped(t *testing.T) {
	if _, err := Load(t.TempDir(), "torznab"); !errors.Is(err, ErrNoCorpus) {
		t.Errorf("expected ErrNoCorpus, got %v", err)
	}
	// An empty directory is also "no corpus", not an empty success.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "torznab"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "torznab"); !errors.Is(err, ErrNoCorpus) {
		t.Errorf("an empty service directory is no corpus, got %v", err)
	}
}

// The point of the whole package: the REAL client transport drives it. A
// harness that hands parsed values to a test proves the harness works.
func TestTheServerSpeaksHTTPToARealClient(t *testing.T) {
	dir := t.TempDir()
	service := filepath.Join(dir, "torznab")
	if err := os.MkdirAll(service, 0o750); err != nil {
		t.Fatal(err)
	}
	writeExchange(t, filepath.Join(service, "search.json"), Exchange{
		Name: "search", Provenance: validProvenance(),
		Request: Request{Method: http.MethodGet, Path: "/api/v1/search?query=arrival"},
		Response: Response{
			Status:  200,
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `[{"title":"Arrival 2160p"}]`,
		},
	})

	corpus, err := Load(dir, "torznab")
	if err != nil {
		t.Fatal(err)
	}
	srv := corpus.Server()
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/search?query=arrival") //nolint:noctx // a test against a local httptest server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q — recorded headers must be replayed", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Arrival") {
		t.Errorf("body = %s", body)
	}
}

// An unmatched request must fail LOUDLY. A 404 is a status a client may well
// handle, which would turn "your test asked for something not in the corpus"
// into "the service said not found" — and the test would then assert on a
// fiction.
func TestAnUnmatchedRequestIsNotAStatusAnyClientHandles(t *testing.T) {
	dir := t.TempDir()
	service := filepath.Join(dir, "torznab")
	if err := os.MkdirAll(service, 0o750); err != nil {
		t.Fatal(err)
	}
	writeExchange(t, filepath.Join(service, "search.json"), Exchange{
		Name: "search", Provenance: validProvenance(),
		Request:  Request{Method: http.MethodGet, Path: "/api/v1/search?query=x"},
		Response: Response{Status: 200, Body: `[]`},
	})
	corpus, err := Load(dir, "torznab")
	if err != nil {
		t.Fatal(err)
	}
	srv := corpus.Server()
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/nothing-like-this") //nolint:noctx // a test against a local httptest server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode < 500 {
		t.Fatalf("status = %d; an unmatched request must not look like a response "+
			"the service could have given", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// And it says what it wanted and what exists, because the alternative is
	// somebody guessing at a path in a corpus they cannot see.
	for _, want := range []string{"no fixture matches", "/api/v1/search?query=x"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the failure should name %q; got:\n%s", want, body)
		}
	}
}

func writeExchange(t *testing.T, path string, e Exchange) {
	t.Helper()
	raw, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The committed Transmission corpus loads, and carries the exchanges a client
// cannot be written without.
//
// It asserts on the CORPUS rather than on a client, because no Transmission
// client exists yet — that is #102. What this pins is that the capture is
// usable: present, well-formed, and containing the two shapes that decide
// whether a client works against a real instance or only against a fixture
// somebody invented.
func TestTheCommittedTransmissionCorpusIsUsable(t *testing.T) {
	corpus, err := Load(corpusDir(), "transmission")
	if errors.Is(err, ErrNoCorpus) {
		t.Skip("no Transmission corpus committed yet")
	}
	if err != nil {
		t.Fatal(err)
	}

	// THE handshake. Transmission answers the first RPC call with 409 and a
	// session id that must be replayed. A client treating 409 as an error
	// works against every hand-written fixture and fails against every real
	// instance — so a corpus without this one is a corpus that cannot catch
	// the mistake it most needs to catch.
	handshake, ok := corpus.Find("session-handshake-409")
	if !ok {
		t.Fatal("no session-handshake-409 — the 409/session-id dance is the one " +
			"exchange a client cannot be written without")
	}
	if handshake.Response.Status != 409 {
		t.Errorf("the handshake recorded status %d, want 409", handshake.Response.Status)
	}
	if handshake.Response.Headers["X-Transmission-Session-Id"] == "" {
		t.Error("the handshake carries no session id header, which is the whole point of it")
	}

	// incomplete-dir is the other gotcha, and this instance HAS it enabled —
	// so a client resolving downloadDir + name mid-transfer gets a path that
	// does not exist. That case is real here rather than theoretical.
	session, ok := corpus.Find("session-get")
	if !ok {
		t.Fatal("no session-get — it carries download-dir and incomplete-dir")
	}
	var decoded struct {
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(session.Response.Body), &decoded); err != nil {
		t.Fatalf("session-get body is not JSON: %v", err)
	}
	for _, field := range []string{
		"download-dir", "incomplete-dir", "incomplete-dir-enabled", "rpc-version",
	} {
		if _, present := decoded.Arguments[field]; !present {
			t.Errorf("session-get does not carry %q, which a client needs to resolve a path", field)
		}
	}

	// Every capture says which version answered. A corpus that cannot say what
	// produced it is one nobody can trust the day it starts failing.
	for _, e := range corpus.Exchanges {
		if e.Provenance.Version == "unknown" || e.Provenance.Version == "" {
			t.Errorf("%s has no recorded version", e.Name)
		}
	}
}

// The finding that only a real capture could produce: a tracker failure is
// INVISIBLE at the top level.
//
// A transfer whose only tracker does not resolve reports error = 0 and
// errorString = "" — the obvious field, and the one its name promises — while
// trackerStats[].lastAnnounceResult says "Could not connect to tracker". So a
// client watching errorString sees a transfer sitting at 0% and looking
// perfectly healthy, forever.
//
// This is pinned as a test rather than left as a comment because it is a claim
// about what the corpus CONTAINS, and a future recapture that lost the stalled
// transfer would quietly remove the only evidence for it.
func TestTheCorpusProvesATrackerFailureIsInvisibleAtTheTopLevel(t *testing.T) {
	corpus, err := Load(corpusDir(), "transmission")
	if errors.Is(err, ErrNoCorpus) {
		t.Skip("no Transmission corpus committed yet")
	}
	if err != nil {
		t.Fatal(err)
	}
	e, ok := corpus.Find("torrent-get")
	if !ok {
		t.Fatal("no torrent-get")
	}

	var decoded struct {
		Arguments struct {
			Torrents []struct {
				Name         string  `json:"name"`
				Status       int     `json:"status"`
				PercentDone  float64 `json:"percentDone"`
				Error        int     `json:"error"`
				ErrorString  string  `json:"errorString"`
				TrackerStats []struct {
					LastAnnounceResult string `json:"lastAnnounceResult"`
				} `json:"trackerStats"`
			} `json:"torrents"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(e.Response.Body), &decoded); err != nil {
		t.Fatalf("torrent-get body is not JSON: %v", err)
	}
	torrents := decoded.Arguments.Torrents
	if len(torrents) < 2 {
		t.Fatalf("the corpus holds %d transfer(s); it needs a completed one AND a "+
			"stalled one for the two paths a poller has to tell apart", len(torrents))
	}

	var sawCompleted, sawSilentlyStalled bool
	for _, tr := range torrents {
		if tr.PercentDone == 1 {
			sawCompleted = true
		}
		var trackerFailed bool
		for _, ts := range tr.TrackerStats {
			if ts.LastAnnounceResult != "" && ts.LastAnnounceResult != "Success" {
				trackerFailed = true
			}
		}
		// The whole point: the tracker failed and the top level says nothing.
		if trackerFailed && tr.Error == 0 && tr.ErrorString == "" {
			sawSilentlyStalled = true
		}
	}
	if !sawCompleted {
		t.Error("no completed transfer — the ingest path has nothing to trigger on")
	}
	if !sawSilentlyStalled {
		t.Error("no transfer whose tracker failed while error/errorString stayed empty — " +
			"that case is the reason trackerStats is captured at all, and without it " +
			"a client watching errorString would pass every test and miss every stall")
	}
}
