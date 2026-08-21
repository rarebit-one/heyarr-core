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
		Service:    "prowlarr",
		Version:    "1.21.2",
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
		Procedure:  "scripts/capture-fixtures.sh prowlarr <endpoint> <key>",
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
	service := filepath.Join(dir, "prowlarr")
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

	if _, err := Load(dir, "prowlarr"); err == nil {
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
	p := validProvenance() // says prowlarr
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
	if _, err := Load(t.TempDir(), "prowlarr"); !errors.Is(err, ErrNoCorpus) {
		t.Errorf("expected ErrNoCorpus, got %v", err)
	}
	// An empty directory is also "no corpus", not an empty success.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "prowlarr"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "prowlarr"); !errors.Is(err, ErrNoCorpus) {
		t.Errorf("an empty service directory is no corpus, got %v", err)
	}
}

// The point of the whole package: the REAL client transport drives it. A
// harness that hands parsed values to a test proves the harness works.
func TestTheServerSpeaksHTTPToARealClient(t *testing.T) {
	dir := t.TempDir()
	service := filepath.Join(dir, "prowlarr")
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

	corpus, err := Load(dir, "prowlarr")
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
	service := filepath.Join(dir, "prowlarr")
	if err := os.MkdirAll(service, 0o750); err != nil {
		t.Fatal(err)
	}
	writeExchange(t, filepath.Join(service, "search.json"), Exchange{
		Name: "search", Provenance: validProvenance(),
		Request:  Request{Method: http.MethodGet, Path: "/api/v1/search?query=x"},
		Response: Response{Status: 200, Body: `[]`},
	})
	corpus, err := Load(dir, "prowlarr")
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
