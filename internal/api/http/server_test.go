// Every HTTP response in this file is closed by the t.Cleanup that h.do
// registers, which bodyclose cannot see through — hence the file-wide
// exemption rather than a comment on each of a few dozen call sites.
//
//nolint:bodyclose // responses are closed by h.do's t.Cleanup
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/drift"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// syncBuffer is a log sink a test can read while the server is still writing
// to it from its own goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness is a running server plus everything a test needs to talk to it.
type harness struct {
	server *httpapi.Server
	store  *auth.Store
	events *events.Log
	logs   *syncBuffer
	cfg    config.Config
	casDir string
}

type harnessOption func(*config.Config)

func withAuthDisabled(c *config.Config) { c.HTTP.Auth.Enabled = false }

func noUnixSocket(c *config.Config) { c.HTTP.UnixSocket = "" }

// newHarness builds a server against a real database and a real CAS directory.
// Nothing here is mocked: the point of most of these tests is the interaction
// between the middleware chain, the router and the credential store, and a mock
// would assert that the test's idea of that interaction is self-consistent.
func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	casDir := filepath.Join(dir, "cas")
	if err := os.MkdirAll(casDir, 0o750); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Peer = config.Peer{Name: "test-peer", Site: "test-site"}
	cfg.HTTP.Addr = "127.0.0.1:0"
	// A short socket path: the sun_path limit is around 104 bytes on macOS and
	// t.TempDir() under a long test name eats most of it.
	cfg.HTTP.UnixSocket = filepath.Join(shortTempDir(t), "h.sock")
	for _, o := range opts {
		o(&cfg)
	}

	store, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}

	logs := &syncBuffer{}
	srv, err := httpapi.New(httpapi.Options{
		Config:             cfg,
		Logger:             slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		DB:                 db,
		Verifier:           verifier,
		Events:             eventLog,
		Build:              buildinfo.Info{Version: "test", Commit: "abc123", Date: "2026-08-20T00:00:00Z"},
		SchemaVersion:      4,
		KnownSchemaVersion: 4,
		CASRoot:            casDir,
		Mount:              []httpapi.MountFunc{testRoutes},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{server: srv, store: store, events: eventLog, logs: logs, cfg: cfg, casDir: casDir}
}

// testRoutes stands in for the resource API that lands on top of this branch.
// It exercises the extension surface exactly as issue #15 and #16 will use it.
func testRoutes(r chi.Router) {
	r.Get("/probe", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/probe", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	r.With(httpapi.RequireScope(auth.ScopeAdmin)).Delete("/probe", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// A read route that a Guest must not reach even at the read floor (ADR-0074),
	// standing in for the per-identity read surface (personal spaces, consumption
	// history) that RefuseGuest guards for real.
	r.With(httpapi.RefuseGuest).Get("/personal", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	r.Get("/boom", func(_ http.ResponseWriter, _ *http.Request) {
		panic("the handler exploded, and the stack must not reach the client")
	})
}

// shutdownBudget is how long a test waits for the server to stop.
//
// It is deliberately far larger than a shutdown ever takes, because it is a
// STARVATION guard rather than a service-level objective. Shutdown itself is
// fast: it closes listeners, drops idle connections and joins two goroutines.
// What made five seconds too short was nothing in this package at all —
// `go test ./...` runs packages in parallel, and the blob soak in
// internal/api/blobs pushes 20 GiB through a loopback connection, saturating a
// core on a two-core runner for the better part of a minute. Under -race, this
// package can then simply not be scheduled for several seconds at a time.
//
// A fixed deadline that a busy machine can miss is a bet on how fast the
// machine is, which is the same mistake as sleeping a fixed duration to wait
// for readiness; four tests in this repository have already failed on CI for
// that. If the server is genuinely wedged, a longer budget still fails — it
// just fails for a reason that is true.
const shutdownBudget = 60 * time.Second

// shortTempDir gives a directory short enough for a unix socket path.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// start brings the server up and waits for it to actually accept connections.
func (h *harness) start(t *testing.T) *harness {
	t.Helper()
	if err := h.server.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
		defer cancel()
		if err := h.server.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	waitForListener(t, "tcp", h.server.Addr())
	if p := h.server.SocketPath(); p != "" {
		waitForListener(t, "unix", p)
	}
	return h
}

// waitForListener polls until the address accepts a connection. It never sleeps
// a fixed duration: three tests in this repo have already failed on CI for
// exactly that, and a fixed wait is a bet on how fast the machine is.
func waitForListener(t *testing.T, network, addr string) {
	t.Helper()
	if addr == "" {
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout(network, addr, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("nothing was listening on %s %s within the deadline", network, addr)
}

func (h *harness) mint(t *testing.T, name string, scopes ...auth.Scope) auth.CreatedToken {
	t.Helper()
	created, err := h.store.Create(context.Background(), name, scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// do issues a request over TCP. The body is closed by a t.Cleanup registered
// here, which is why the call sites carry a bodyclose exemption: the linter
// cannot see through the helper.
func (h *harness) do(t *testing.T, method, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, "http://"+h.server.Addr()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeProblem(t *testing.T, resp *http.Response) problem.Problem {
	t.Helper()
	if got := resp.Header.Get("Content-Type"); got != problem.MediaType {
		t.Errorf("Content-Type = %q, want %q", got, problem.MediaType)
	}
	var p problem.Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("the error body is not a problem document: %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// ADR-0011: the refusal
// ---------------------------------------------------------------------------

// The rule is "this process does not open an unauthenticated socket on a
// routable address". config.Validate refuses the same configuration, and that
// is not enough: a validator holds only until something constructs a server
// another way. This drives the server constructor directly.
func TestListenerConstructionRefusesAnUnauthenticatedPublicBind(t *testing.T) {
	tests := []struct {
		name       string
		addr       string
		authOn     bool
		wantRefuse bool
	}{
		{name: "wildcard v4 without auth", addr: "0.0.0.0:0", wantRefuse: true},
		{name: "bare port without auth", addr: ":0", wantRefuse: true},
		{name: "wildcard v6 without auth", addr: "[::]:0", wantRefuse: true},
		{name: "a routable address without auth", addr: "192.0.2.1:0", wantRefuse: true},
		{name: "an unresolvable host is treated as routable", addr: "not-a-host:0", wantRefuse: true},
		{name: "loopback without auth is allowed", addr: "127.0.0.1:0"},
		{name: "localhost without auth is allowed", addr: "localhost:0"},
		{name: "wildcard WITH auth is allowed", addr: "127.0.0.1:0", authOn: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, func(c *config.Config) {
				c.HTTP.Addr = tt.addr
				c.HTTP.Auth.Enabled = tt.authOn
			})
			err := h.server.Start()
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
				defer cancel()
				_ = h.server.Shutdown(ctx)
			}

			if !tt.wantRefuse {
				if err != nil {
					t.Fatalf("Start refused a legal configuration: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the server bound an unauthenticated non-loopback address")
			}
			// "It failed" is not enough. An operator has to be able to fix it
			// from the message alone, so it must name the address, the setting
			// and the way out.
			msg := err.Error()
			for _, want := range []string{"refusing to start", tt.addr, "http.auth.enabled", "127.0.0.1"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal does not mention %q: %s", want, msg)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Authentication and authorisation
// ---------------------------------------------------------------------------

// Every response carries X-Content-Type-Options, whether or not the handler
// that produced it remembered to set it.
//
// Three writers set it individually today — routes.go's writeJSON,
// problem.Write and the resource API's write — and the failure mode of three
// places remembering the same thing is a fourth that does not. This asserts
// the guarantee is structural rather than habitual: `/probe` sets no headers
// at all, and the panic on `/boom` never reaches a writer that could.
//
// It is defence in depth and not the primary control. The primary control is
// that bodies are encoding/json output, which HTML-escapes by default. nosniff
// stops a browser deciding for itself that an application/json response is
// really HTML, which matters only if the escaping were ever bypassed.
func TestEveryResponseIsNosniff(t *testing.T) {
	h := newHarness(t, withAuthDisabled).start(t)

	for _, tc := range []struct {
		name, method, path string
	}{
		{"a health probe", http.MethodGet, "/healthz"},
		{"a readiness probe", http.MethodGet, "/readyz"},
		// A handler that sets no headers of its own.
		{"a bare handler", http.MethodGet, "/api/v1/probe"},
		// A problem document from the NotFound handler.
		{"an unmatched route", http.MethodGet, "/api/v1/there-is-no-such-route"},
		// A problem document from MethodNotAllowed.
		{"a disallowed method", http.MethodPatch, "/api/v1/probe"},
		// The path least likely to have remembered: nothing wrote a header
		// before the panic, and recovery writes the response.
		{"a panicking handler", http.MethodGet, "/api/v1/boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(t, tc.method, tc.path, "")
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff (status %d)",
					got, resp.StatusCode)
			}
			// And nothing in this server answers as HTML, which is the other
			// half of why the JSON bodies are safe.
			if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
				t.Errorf("Content-Type = %q; this API never answers as HTML", ct)
			}
		})
	}
}

func TestScopeEnforcement(t *testing.T) {
	h := newHarness(t).start(t)
	readOnly := h.mint(t, "reader", auth.ScopeRead)
	writer := h.mint(t, "writer", auth.ScopeWrite)
	admin := h.mint(t, "admin", auth.ScopeAdmin)

	tests := []struct {
		name       string
		method     string
		path       string
		token      string
		wantStatus int
		wantType   string
	}{
		{"no credential on a read route", http.MethodGet, "/api/v1/probe", "", 401, problem.TypeUnauthorized},
		{"no credential on /api/v1/system", http.MethodGet, "/api/v1/system", "", 401, problem.TypeUnauthorized},
		{"read token on a read route", http.MethodGet, "/api/v1/probe", readOnly.Secret, 200, ""},
		{"read token on a write route", http.MethodPost, "/api/v1/probe", readOnly.Secret, 403, problem.TypeForbidden},
		{"read token on an admin route", http.MethodDelete, "/api/v1/probe", readOnly.Secret, 403, problem.TypeForbidden},
		{"write token on a write route", http.MethodPost, "/api/v1/probe", writer.Secret, 201, ""},
		{"write implies read", http.MethodGet, "/api/v1/probe", writer.Secret, 200, ""},
		{"write token on an admin route", http.MethodDelete, "/api/v1/probe", writer.Secret, 403, problem.TypeForbidden},
		{"admin implies everything", http.MethodDelete, "/api/v1/probe", admin.Secret, 204, ""},
		{"a garbage credential", http.MethodGet, "/api/v1/probe", "heyarr_nonsense", 401, problem.TypeUnauthorized},
		{"an unknown route", http.MethodGet, "/api/v1/nope", admin.Secret, 404, problem.TypeNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.do(t, tt.method, tt.path, tt.token)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantType == "" {
				return
			}
			p := decodeProblem(t, resp)
			if p.Type != tt.wantType {
				t.Errorf("problem type = %q, want %q — clients branch on this", p.Type, tt.wantType)
			}
			if p.Status != tt.wantStatus {
				t.Errorf("problem status = %d, want %d", p.Status, tt.wantStatus)
			}
			if p.RequestID == "" {
				t.Error("the problem document carries no request id, so nobody can correlate it with the log")
			}
		})
	}
}

func TestRevokedAndExpiredTokensAreRejected(t *testing.T) {
	h := newHarness(t).start(t)
	ctx := context.Background()

	revoked := h.mint(t, "revoked", auth.ScopeRead)
	if resp := h.do(t, http.MethodGet, "/api/v1/probe", revoked.Secret); resp.StatusCode != 200 {
		t.Fatalf("the token did not work before revocation: %d", resp.StatusCode)
	}
	if _, err := h.store.Revoke(ctx, revoked.Token.ID); err != nil {
		t.Fatal(err)
	}
	resp := h.do(t, http.MethodGet, "/api/v1/probe", revoked.Secret)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked token got %d, want 401", resp.StatusCode)
	}
	if p := decodeProblem(t, resp); p.Type != problem.TypeUnauthorized {
		t.Errorf("problem type = %q", p.Type)
	}

	past := time.Now().UTC().Add(-time.Minute)
	expired, err := h.store.Create(ctx, "expired", []auth.Scope{auth.ScopeRead}, &past)
	if err != nil {
		t.Fatal(err)
	}
	if resp := h.do(t, http.MethodGet, "/api/v1/probe", expired.Secret); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an expired token got %d, want 401", resp.StatusCode)
	}
}

func TestARejectionSaysNothingAboutWhy(t *testing.T) {
	// "No such token" and "wrong secret" are different facts; telling an
	// unauthorised caller which one applies is free reconnaissance.
	h := newHarness(t).start(t)
	real := h.mint(t, "svc", auth.ScopeRead)

	unknown := decodeProblem(t, h.do(t, http.MethodGet, "/api/v1/probe",
		auth.TokenPrefix+strings.Repeat("a", 26)+"_"+strings.Repeat("b", 52)))
	tampered := []byte(real.Secret)
	tampered[len(tampered)-1] ^= 1
	wrongSecret := decodeProblem(t, h.do(t, http.MethodGet, "/api/v1/probe", string(tampered)))

	if unknown.Detail != wrongSecret.Detail || unknown.Type != wrongSecret.Type {
		t.Errorf("the two rejections are distinguishable: %q vs %q", unknown.Detail, wrongSecret.Detail)
	}
}

func TestAuthDisabledOpensEverythingOnLoopback(t *testing.T) {
	h := newHarness(t, withAuthDisabled).start(t)
	for _, tt := range []struct {
		method string
		want   int
	}{{http.MethodGet, 200}, {http.MethodPost, 201}, {http.MethodDelete, 204}} {
		if resp := h.do(t, tt.method, "/api/v1/probe", ""); resp.StatusCode != tt.want {
			t.Errorf("%s with auth disabled = %d, want %d", tt.method, resp.StatusCode, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// The access log
// ---------------------------------------------------------------------------

// A log is the most widely copied artefact a system produces — it goes into
// tickets, gists and bug reports. A credential in it is a credential leaked.
func TestTheAccessLogNeverRecordsATokenValue(t *testing.T) {
	h := newHarness(t).start(t)
	valid := h.mint(t, "logged", auth.ScopeRead)
	revoked := h.mint(t, "revoked", auth.ScopeRead)
	if _, err := h.store.Revoke(context.Background(), revoked.Token.ID); err != nil {
		t.Fatal(err)
	}

	forged := auth.TokenPrefix + strings.Repeat("c", 26) + "_" + strings.Repeat("d", 52)

	// Every path a credential could take through the server: the happy one,
	// two rejection paths, a panicking handler, an unmatched route, and the
	// query string — because a client that puts a token in a query parameter
	// must not thereby write it into the log.
	cases := []struct{ method, path, token string }{
		{http.MethodGet, "/api/v1/probe", valid.Secret},
		{http.MethodPost, "/api/v1/probe", valid.Secret},              // 403 path
		{http.MethodGet, "/api/v1/probe", revoked.Secret},             // 401 path
		{http.MethodGet, "/api/v1/probe", forged},                     // 401 path
		{http.MethodGet, "/api/v1/boom", valid.Secret},                // panic path
		{http.MethodGet, "/api/v1/does-not-exist", valid.Secret},      // 404 path
		{http.MethodGet, "/api/v1/probe?token=" + valid.Secret, ""},   // query string
		{http.MethodGet, "/healthz?access_token=" + valid.Secret, ""}, // unauthenticated route
	}
	for _, c := range cases {
		h.do(t, c.method, c.path, c.token)
	}

	logs := h.logs.String()
	if logs == "" {
		t.Fatal("nothing was logged at all, so this test proves nothing")
	}
	for _, secret := range []string{valid.Secret, revoked.Secret, forged} {
		if strings.Contains(logs, secret) {
			t.Errorf("a token appears in the access log: %s", secret)
		}
		// The secret half on its own, in case only the prefix was stripped.
		half := secret[strings.LastIndex(secret, "_")+1:]
		if strings.Contains(logs, half) {
			t.Errorf("a token secret appears in the access log: %s", half)
		}
	}
	if strings.Contains(strings.ToLower(logs), "authorization") {
		t.Error("the Authorization header is named in the log — headers must never be dumped")
	}

	// And the log has to be useful, or the safe way to pass this test is to log
	// nothing at all.
	for _, want := range []string{`"query_keys":"token"`, `"principal":"logged"`, `"token_id"`, `"route"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("the access log is missing %s — it must stay useful, not just safe", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Panic recovery
// ---------------------------------------------------------------------------

func TestAPanickingHandlerYieldsAProblemAndTheServerKeepsServing(t *testing.T) {
	h := newHarness(t).start(t)
	token := h.mint(t, "svc", auth.ScopeRead)

	resp := h.do(t, http.MethodGet, "/api/v1/boom", token.Secret)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// The body must be a problem document, and must not be a stack trace.
	var p problem.Problem
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("the body is not a problem document: %s", body)
	}
	if p.Type != problem.TypeInternal {
		t.Errorf("problem type = %q, want %q", p.Type, problem.TypeInternal)
	}
	for _, leak := range []string{"goroutine", "runtime.", ".go:", "the handler exploded"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("the response body leaks %q:\n%s", leak, body)
		}
	}

	// The stack must reach the log, or the 500 is unaggregatable.
	logs := h.logs.String()
	if !strings.Contains(logs, "handler panicked") || !strings.Contains(logs, "goroutine") {
		t.Error("the panic stack never reached the log")
	}

	// And the next request must still work: a recovery that kills the server
	// is not a recovery.
	if resp := h.do(t, http.MethodGet, "/api/v1/probe", token.Secret); resp.StatusCode != 200 {
		t.Errorf("the server stopped serving after a panic: %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Listeners
// ---------------------------------------------------------------------------

func TestTCPAndUnixSocketServeTheSameHandler(t *testing.T) {
	h := newHarness(t).start(t)
	token := h.mint(t, "svc", auth.ScopeRead)

	overTCP := h.do(t, http.MethodGet, "/api/v1/system", token.Secret)
	tcpBody, err := io.ReadAll(overTCP.Body)
	if err != nil {
		t.Fatal(err)
	}

	socketClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", h.server.SocketPath())
		},
	}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://localhost/api/v1/system", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token.Secret)
	overUnix, err := socketClient.Do(req)
	if err != nil {
		t.Fatalf("the unix socket did not serve the request: %v", err)
	}
	defer func() { _ = overUnix.Body.Close() }()
	unixBody, err := io.ReadAll(overUnix.Body)
	if err != nil {
		t.Fatal(err)
	}

	if overTCP.StatusCode != overUnix.StatusCode {
		t.Errorf("tcp returned %d and the socket returned %d", overTCP.StatusCode, overUnix.StatusCode)
	}
	if string(tcpBody) != string(unixBody) {
		t.Errorf("the two listeners returned different bodies:\n tcp: %s\nunix: %s", tcpBody, unixBody)
	}
}

func TestTheSocketIsRestrictedAndRemovedOnShutdown(t *testing.T) {
	h := newHarness(t)
	if err := h.server.Start(); err != nil {
		t.Fatal(err)
	}
	path := h.server.SocketPath()
	waitForListener(t, "unix", path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("the socket is mode %o — anything on the machine can talk to it", perm)
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	if err := h.server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the socket file survived shutdown, so the next start will trip over it")
	}
	// Shutting down twice must be safe: the supervisor and a deferred cleanup
	// can both reach it.
	if err := h.server.Shutdown(ctx); err != nil {
		t.Errorf("the second Shutdown returned %v", err)
	}
}

func TestAStaleSocketDoesNotPreventStartup(t *testing.T) {
	dir := shortTempDir(t)
	stale := filepath.Join(dir, "h.sock")

	// A socket file left behind by a process that was killed: the path exists
	// and nothing is listening on it.
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("the test did not manage to leave a stale file: %v", err)
	}

	h := newHarness(t, func(c *config.Config) { c.HTTP.UnixSocket = stale })
	h.start(t)

	if h.server.SocketPath() != stale {
		t.Fatalf("the server bound %q, want %q", h.server.SocketPath(), stale)
	}
	if !strings.Contains(h.logs.String(), "stale unix socket") {
		t.Error("the stale socket was removed silently — an operator should see it happened")
	}
}

func TestALiveSocketIsNotStolenFromAnotherProcess(t *testing.T) {
	first := newHarness(t).start(t)

	second := newHarness(t, func(c *config.Config) {
		c.HTTP.Addr = "127.0.0.1:0"
		c.HTTP.UnixSocket = first.server.SocketPath()
	})
	err := second.server.Start()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
		defer cancel()
		_ = second.server.Shutdown(ctx)
		t.Fatal("a second server stole a socket that was still being served")
	}
	if !strings.Contains(err.Error(), "already served") {
		t.Errorf("error = %v, want it to say the socket is in use", err)
	}
}

func TestAServerWithNoListenerAtAllIsRefused(t *testing.T) {
	h := newHarness(t, noUnixSocket, func(c *config.Config) { c.HTTP.Addr = "" })
	if err := h.server.Start(); err == nil {
		t.Fatal("a server with neither a TCP address nor a socket started")
	}
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

func TestHealthAndReadiness(t *testing.T) {
	h := newHarness(t).start(t)

	// Liveness is unauthenticated and says nothing about dependencies.
	resp := h.do(t, http.MethodGet, "/healthz", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}

	resp = h.do(t, http.MethodGet, "/readyz", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200", resp.StatusCode)
	}
	var ready httpapi.Readiness
	if err := json.NewDecoder(resp.Body).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	if ready.Status != "ready" || len(ready.Checks) != 2 {
		t.Fatalf("readiness = %+v", ready)
	}
}

func TestReadinessFailsWhenTheCASRootIsGone(t *testing.T) {
	h := newHarness(t).start(t)
	if err := os.RemoveAll(h.casDir); err != nil {
		t.Fatal(err)
	}

	resp := h.do(t, http.MethodGet, "/readyz", "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d with no CAS root, want 503 — a load balancer would keep routing to it", resp.StatusCode)
	}
	var ready httpapi.Readiness
	if err := json.NewDecoder(resp.Body).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	if ready.Status != "not ready" {
		t.Errorf("status = %q", ready.Status)
	}
	// /readyz is unauthenticated, so it must not name paths.
	body, _ := json.Marshal(ready)
	if strings.Contains(string(body), h.casDir) {
		t.Errorf("the unauthenticated readiness body names a filesystem path: %s", body)
	}
}

func TestSystemReportsIdentityAndDependencies(t *testing.T) {
	h := newHarness(t).start(t)
	token := h.mint(t, "svc", auth.ScopeRead)

	resp := h.do(t, http.MethodGet, "/api/v1/system", token.Secret)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got httpapi.SystemInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Build.Version != "test" || got.Build.Commit != "abc123" {
		t.Errorf("build = %+v", got.Build)
	}
	if got.Peer.Name != "test-peer" || got.Peer.Site != "test-site" {
		t.Errorf("peer = %+v", got.Peer)
	}
	if got.SchemaVersion != 4 {
		t.Errorf("schema_version = %d, want 4", got.SchemaVersion)
	}
	if !got.Database.OK || !got.CAS.OK {
		t.Errorf("dependencies reported unhealthy: %+v %+v", got.Database, got.CAS)
	}
	if !got.AuthEnabled {
		t.Error("auth_enabled is false on a server with auth on")
	}
}

func TestMetricsRequireACredentialAndCountRequests(t *testing.T) {
	h := newHarness(t).start(t)
	token := h.mint(t, "scraper", auth.ScopeRead)

	if resp := h.do(t, http.MethodGet, "/metrics", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/metrics without a credential = %d, want 401 — it leaks route names and traffic shape",
			resp.StatusCode)
	}

	h.do(t, http.MethodGet, "/api/v1/probe", token.Secret)
	resp := h.do(t, http.MethodGet, "/metrics", token.Secret)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"heyarr_http_requests_total",
		"heyarr_http_request_duration_seconds",
		"heyarr_http_requests_in_flight",
		`route="/api/v1/probe"`,
		`status_class="2xx"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics is missing %s", want)
		}
	}
	// The label is the route pattern, never the path: a series per blob hash
	// would take the metrics endpoint down and Prometheus with it.
	if strings.Contains(string(body), `route="/api/v1/does-not-exist"`) {
		t.Error("a raw path was used as a metric label")
	}
	// A private registry, not the global default one.
	if strings.Contains(string(body), "go_goroutines") {
		t.Error("the default Go collectors are published, so this is the global registry")
	}
}

func TestMetricsAreOpenWhenAuthenticationIsDisabled(t *testing.T) {
	// Documented rule: /metrics is authenticated exactly like the API. With
	// authentication off — which is only legal on loopback, and the server
	// refuses to start otherwise — it is open, so there is no configuration in
	// which it is reachable unauthenticated from off the machine.
	h := newHarness(t, withAuthDisabled).start(t)
	if resp := h.do(t, http.MethodGet, "/metrics", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("/metrics with auth disabled = %d, want 200", resp.StatusCode)
	}
}

func TestTheRequestIDIsEchoedAndSanitised(t *testing.T) {
	h := newHarness(t).start(t)

	tests := []struct {
		name     string
		sent     string
		wantEcho bool
	}{
		{"a sane id is honoured", "abc-123", true},
		{"a newline is rejected", "abc\ndef", false},
		{"an over-long id is rejected", strings.Repeat("x", 200), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
				"http://"+h.server.Addr()+"/healthz", nil)
			if err != nil {
				t.Fatal(err)
			}
			// A header value with a newline cannot be sent through net/http at
			// all, which is itself a defence; set it the only way it can be.
			req.Header[httpapi.RequestIDHeader] = []string{tt.sent}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				if !tt.wantEcho {
					return // the client refused to send it, which is fine
				}
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()

			got := resp.Header.Get(httpapi.RequestIDHeader)
			if got == "" {
				t.Fatal("no request id was returned")
			}
			if tt.wantEcho && got != tt.sent {
				t.Errorf("request id = %q, want the inbound %q", got, tt.sent)
			}
			if !tt.wantEcho && got == tt.sent {
				t.Errorf("a hostile inbound request id was echoed: %q", got)
			}
		})
	}
}

func TestShutdownIsCleanAndReportsNoError(t *testing.T) {
	h := newHarness(t)
	if err := h.server.Start(); err != nil {
		t.Fatal(err)
	}
	waitForListener(t, "tcp", h.server.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	if err := h.server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-h.server.Err():
		t.Fatalf("a clean shutdown reported a serving error: %v", err)
	default:
	}

	// Nothing must still be listening.
	if _, err := net.DialTimeout("tcp", h.server.Addr(), 250*time.Millisecond); err == nil {
		t.Error("the TCP listener survived shutdown")
	}
}

func TestHandlerIsUsableWithoutBinding(t *testing.T) {
	// The dependent branches test their routes through Handler() rather than
	// binding a port; this asserts that surface works.
	h := newHarness(t)
	if h.server.Handler() == nil {
		t.Fatal("Handler() returned nil")
	}
	if h.server.Registry() == nil {
		t.Fatal("Registry() returned nil")
	}
	fmt.Fprint(io.Discard, h.server.Addr())
}

func TestAnOverlongSocketPathDoesNotTakeTheServerDown(t *testing.T) {
	// sun_path is a fixed-size array in a C struct, so a data directory nested
	// a few levels deep produces "bind: invalid argument" and no explanation.
	// Losing the local transport is bad; refusing to serve at all because a
	// path is long is worse.
	deep := filepath.Join(shortTempDir(t), strings.Repeat("nested/", 30))
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(deep, "heyarr.sock")

	h := newHarness(t, func(c *config.Config) { c.HTTP.UnixSocket = long })
	if err := h.server.Start(); err != nil {
		t.Fatalf("Start failed over a socket path length: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
		defer cancel()
		_ = h.server.Shutdown(ctx)
	})
	waitForListener(t, "tcp", h.server.Addr())

	if h.server.SocketPath() != "" {
		t.Errorf("the server claims to have bound %q", h.server.SocketPath())
	}
	if !strings.Contains(h.logs.String(), "too long") {
		t.Error("the skipped socket was not explained in the log")
	}
	if resp := h.do(t, http.MethodGet, "/healthz", ""); resp.StatusCode != 200 {
		t.Errorf("the TCP listener is not serving: %d", resp.StatusCode)
	}
}

func TestAnOverlongSocketPathIsFatalWhenItIsTheOnlyListener(t *testing.T) {
	deep := filepath.Join(shortTempDir(t), strings.Repeat("nested/", 30))
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, func(c *config.Config) {
		c.HTTP.Addr = ""
		c.HTTP.UnixSocket = filepath.Join(deep, "heyarr.sock")
	})
	err := h.server.Start()
	if err == nil {
		t.Fatal("the server started with no reachable listener at all")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("error = %v, want it to name the path length", err)
	}
}

// The event log's head is what makes "follow the stream from now" expressible.
// Without it a client's only options are to replay from sequence zero or to
// guess, and both get worse the longer the instance has been up (§76,
// ADR-0009).
//
// Zero for an empty log is the deliberate choice, not an absence: ?after=0
// already means "everything", so the empty case needs no special handling at
// the client.
func TestSystemReportsTheEventLogHead(t *testing.T) {
	h := newHarness(t).start(t)
	token := h.mint(t, "svc", auth.ScopeRead)

	if got := h.systemInfo(t, token.Secret).Events; got.Head != 0 || !got.OK {
		t.Fatalf("a fresh instance reports events = %+v, want head 0 and ok", got)
	}

	var last int64
	for i := range 3 {
		e, err := h.events.Emit(t.Context(), events.TypeBlobCreated, "blob", fmt.Sprintf("b%d", i), nil)
		if err != nil {
			t.Fatal(err)
		}
		last = e.Seq
	}

	got := h.systemInfo(t, token.Secret).Events
	if !got.OK {
		t.Fatalf("events.ok is false against a healthy log: %+v", got)
	}
	if got.Head != last {
		t.Errorf("events.head = %d, want %d — the head must be the most recent sequence, "+
			"not a count and not the next one", got.Head, last)
	}
}

// failingEventHead stands in for a log that cannot be read.
type failingEventHead struct{}

func (failingEventHead) Latest(context.Context) (int64, error) {
	return 0, errors.New("the event log is unreadable")
}

// A head that could not be read must not present itself as an empty log.
//
// Zero is a legitimate head, so a client cannot tell "nothing has happened yet"
// from "we could not find out" unless something says so — and a client that
// resumed from a fabricated 0 would replay the entire backlog it was trying to
// skip. Reporting rather than failing the whole request matches what this
// endpoint already does for the database and the CAS: /api/v1/system exists to
// describe a degraded node, not to become unavailable alongside it.
func TestSystemReportsAnUnreadableEventLogRatherThanZero(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	cfg.HTTP.Auth.Enabled = false

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, DB: db, Events: failingEventHead{},
		Logger: slog.New(slog.DiscardHandler), KnownSchemaVersion: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/system", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a node with an unreadable log still has an identity to report",
			resp.StatusCode)
	}
	var got httpapi.SystemInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Events.OK {
		t.Error("events.ok is true after the head could not be read")
	}
	if got.Events.Head != 0 {
		t.Errorf("events.head = %d after a failed read, want 0", got.Events.Head)
	}
}

// A server wired without a log would report head 0 forever, which reads as an
// empty log and sends any client that trusted it back to sequence zero. There
// is no configuration in which that field should be a guess, so it is refused
// at construction rather than defaulted.
func TestNewRequiresAnEventLog(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	cfg.HTTP.Auth.Enabled = false

	if _, err := httpapi.New(httpapi.Options{Config: cfg, DB: db}); err == nil {
		t.Fatal("a server was built with no event log")
	}
}

// systemInfo reads GET /api/v1/system.
func (h *harness) systemInfo(t *testing.T, token string) httpapi.SystemInfo {
	t.Helper()
	resp := h.do(t, http.MethodGet, "/api/v1/system", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/v1/system = %d", resp.StatusCode)
	}
	var got httpapi.SystemInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// A degraded node must be able to say it is degraded. A Heyarr with no ffprobe
// still scans, ingests and serves — but probe jobs then sit pending forever,
// and "why is nothing probing" should be one request rather than an
// investigation (ADR-0023).
func TestSystemReportsTheMediaToolchain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		media []httpapi.ToolInfo
		check func(*testing.T, []httpapi.ToolInfo)
	}{
		{
			name: "available",
			media: []httpapi.ToolInfo{
				{Name: "ffprobe", Path: "/opt/bin/ffprobe", Version: "7.0.2", Available: true},
				{Name: "ffmpeg", Path: "/opt/bin/ffmpeg", Version: "7.0.2", Available: true},
			},
			check: func(t *testing.T, got []httpapi.ToolInfo) {
				t.Helper()
				if len(got) != 2 {
					t.Fatalf("media = %+v, want two tools", got)
				}
				if !got[0].Available || got[0].Version != "7.0.2" || got[0].Path == "" {
					t.Errorf("ffprobe = %+v", got[0])
				}
			},
		},
		{
			name: "absent",
			media: []httpapi.ToolInfo{
				{Name: "ffprobe", Detail: "not found on PATH"},
				{Name: "ffmpeg", Detail: "not found on PATH"},
			},
			check: func(t *testing.T, got []httpapi.ToolInfo) {
				t.Helper()
				for _, tool := range got {
					if tool.Available {
						t.Errorf("%s reported available on a bare node", tool.Name)
					}
					if tool.Detail == "" {
						t.Errorf("%s is unavailable and says nothing about why", tool.Name)
					}
					// An unavailable tool must not carry a path or a version:
					// a client rendering "ffprobe 7.0.2 (unavailable)" has
					// been handed a contradiction.
					if tool.Path != "" || tool.Version != "" {
						t.Errorf("%s is unavailable but reports %+v", tool.Name, tool)
					}
				}
			},
		},
		{
			// Nothing wired at all. The field must still be a list, because a
			// client should not have to handle both null and [] for the same
			// "nothing here".
			name:  "unwired",
			media: nil,
			check: func(t *testing.T, got []httpapi.ToolInfo) {
				t.Helper()
				if got == nil {
					t.Error("media is null rather than an empty list")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, info := systemWithMedia(t, tc.media)
			tc.check(t, info.Media)
			if !strings.Contains(string(raw), `"media"`) {
				t.Errorf("the system body has no media key: %s", raw)
			}
		})
	}
}

// systemWithMedia builds a minimal server reporting the given toolchain and
// reads GET /api/v1/system from it.
func systemWithMedia(t *testing.T, tools []httpapi.ToolInfo) ([]byte, httpapi.SystemInfo) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	cfg.HTTP.Auth.Enabled = false

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, DB: db, Events: eventLog, Media: tools,
		Logger: slog.New(slog.DiscardHandler), KnownSchemaVersion: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/system", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var info httpapi.SystemInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	return raw, info
}

// ---------------------------------------------------------------------------
// Drift (#150)
// ---------------------------------------------------------------------------
//
// #132 found a deployment 36 commits and seven migrations behind main, across
// two whole milestones, that nothing would have reported. It also found WHY
// nobody caught it: the verification asked for the SILENCE of a warning, and
// the warning did not exist in the build being verified. The silence was
// perfect and meant nothing.
//
// So every "no drift" assertion below is preceded by the SAME assertion
// watching drift fire on the same endpoint. The silence is only allowed to
// count once the noise has been heard.

// systemResponse builds a minimal server with a chosen build identity and
// schema state, and reads GET /api/v1/system from it.
func systemResponse(t *testing.T, build buildinfo.Info, applied, known int64, query string,
) (int, []byte) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	cfg.HTTP.Auth.Enabled = false

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, DB: db, Events: eventLog,
		Logger:        slog.New(slog.DiscardHandler),
		Build:         build,
		SchemaVersion: applied, KnownSchemaVersion: known,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/system"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

// systemDrift reads the drift report from GET /api/v1/system, insisting on 200.
func systemDrift(t *testing.T, build buildinfo.Info, applied, known int64, query string) drift.Report {
	t.Helper()
	status, raw := systemResponse(t, build, applied, known, query)
	if status != http.StatusOK {
		t.Fatalf("/api/v1/system%s = %d: %s", query, status, raw)
	}
	var info httpapi.SystemInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("decoding /api/v1/system: %v\n%s", err, raw)
	}
	return info.Drift
}

// The expected build for these tests. A real semantic version, so the distance
// is a number rather than "different by an unknown amount".
var (
	deployedBuild = buildinfo.Info{Version: "v1.2.0", Commit: "324a0fc1e2d3a4b5"}
	shippedBuild  = buildinfo.Info{Version: "v1.4.0", Commit: "950ec9d5f6a7b8c9"}
)

func expectation(b buildinfo.Info) string {
	return "?expected_version=" + b.Version + "&expected_commit=" + b.Commit
}

// TestSystemReportsBuildDriftThenSilence is the A/B for the build half over
// HTTP. It asserts the DISTANCE, not that something appeared.
func TestSystemReportsBuildDriftThenSilence(t *testing.T) {
	// A: a node running two minor versions behind what was shipped.
	fired := systemDrift(t, deployedBuild, 4, 4, expectation(shippedBuild))
	if fired.Build.Status != drift.StatusBehind {
		t.Fatalf("build status = %q, want %q — the drift case did not fire, so the "+
			"silence asserted below would prove nothing", fired.Build.Status, drift.StatusBehind)
	}
	if fired.Build.MinorBehind != 2 {
		t.Fatalf("minor_behind = %d, want 2", fired.Build.MinorBehind)
	}
	if fired.Build.Actual.Version != deployedBuild.Version {
		t.Errorf("the report does not say what is running: %+v", fired.Build)
	}
	if fired.Build.Expected.Version != shippedBuild.Version {
		t.Errorf("the report does not say what was expected: %+v", fired.Build)
	}

	// B: the same request against a node at the shipped build.
	quiet := systemDrift(t, shippedBuild, 4, 4, expectation(shippedBuild))
	if quiet.Build.Status != drift.StatusCurrent {
		t.Errorf("build status = %q, want %q", quiet.Build.Status, drift.StatusCurrent)
	}
	if quiet.Build.MajorBehind != 0 || quiet.Build.MinorBehind != 0 || quiet.Build.PatchBehind != 0 {
		t.Errorf("a current node reports a distance: %+v", quiet.Build)
	}
}

// TestSystemReportsSchemaDriftThenSilence is the A/B for the schema half, and
// it is the one the deployment in #132 needed: the binary is irrelevant here,
// only what has and has not been applied to the database.
func TestSystemReportsSchemaDriftThenSilence(t *testing.T) {
	// A: eleven applied, eighteen embedded — the seven migrations of #132.
	fired := systemDrift(t, shippedBuild, 11, 18, "")
	if fired.Schema.Status != drift.StatusBehind {
		t.Fatalf("schema status = %q, want %q — the drift case did not fire",
			fired.Schema.Status, drift.StatusBehind)
	}
	if fired.Schema.MigrationsBehind != 7 {
		t.Fatalf("migrations_behind = %d, want 7", fired.Schema.MigrationsBehind)
	}
	if fired.Schema.Applied != 11 || fired.Schema.Expected != 18 {
		t.Errorf("the report does not carry both versions: %+v", fired.Schema)
	}

	// B: and now the same endpoint with every migration applied.
	quiet := systemDrift(t, shippedBuild, 18, 18, "")
	if quiet.Schema.Status != drift.StatusCurrent {
		t.Errorf("schema status = %q, want %q", quiet.Schema.Status, drift.StatusCurrent)
	}
	if quiet.Schema.MigrationsBehind != 0 || quiet.Schema.MigrationsAhead != 0 {
		t.Errorf("a current schema reports a distance: %+v", quiet.Schema)
	}
}

// TestSystemReportsTheTwoDriftsIndependently is why they are two fields.
//
// A current binary with unapplied migrations is not a mild version of being
// behind — it is a build running against a schema it was never tested on — and
// a stale binary against a fully migrated database is the other failure. One
// combined flag would let either hide the other.
func TestSystemReportsTheTwoDriftsIndependently(t *testing.T) {
	t.Run("a current binary with seven migrations unapplied", func(t *testing.T) {
		got := systemDrift(t, shippedBuild, 11, 18, expectation(shippedBuild))
		if got.Build.Status != drift.StatusCurrent {
			t.Errorf("build status = %q, want %q", got.Build.Status, drift.StatusCurrent)
		}
		if got.Schema.Status != drift.StatusBehind || got.Schema.MigrationsBehind != 7 {
			t.Errorf("schema drift was hidden by a current build: %+v", got.Schema)
		}
	})

	t.Run("a stale binary against a fully migrated database", func(t *testing.T) {
		got := systemDrift(t, deployedBuild, 18, 18, expectation(shippedBuild))
		if got.Schema.Status != drift.StatusCurrent {
			t.Errorf("schema status = %q, want %q", got.Schema.Status, drift.StatusCurrent)
		}
		if got.Build.Status != drift.StatusBehind || got.Build.MinorBehind != 2 {
			t.Errorf("build drift was hidden by a current schema: %+v", got.Build)
		}
	})

	t.Run("a database migrated by a newer build than this one", func(t *testing.T) {
		got := systemDrift(t, deployedBuild, 18, 11, expectation(deployedBuild))
		if got.Schema.Status != drift.StatusAhead || got.Schema.MigrationsAhead != 7 {
			t.Errorf("schema = %+v, want ahead by 7", got.Schema)
		}
		if got.Build.Status != drift.StatusCurrent {
			t.Errorf("build status = %q, want %q", got.Build.Status, drift.StatusCurrent)
		}
	})
}

// A caller that supplies no expectation must be told the build comparison was
// not made. "unknown" reported as "current" is the whole failure of #132 in one
// field: a check that has stopped comparing looks exactly like a fleet that
// never drifts.
func TestSystemReportsUnknownBuildDriftRatherThanCurrent(t *testing.T) {
	got := systemDrift(t, deployedBuild, 4, 4, "")
	if got.Build.Status != drift.StatusUnknown {
		t.Errorf("build status = %q, want %q", got.Build.Status, drift.StatusUnknown)
	}
	if got.Build.Detail == "" {
		t.Error("an unknown build comparison says nothing about why")
	}
	// The schema half still answers, because it needs nothing from the caller.
	if got.Schema.Status != drift.StatusCurrent {
		t.Errorf("schema status = %q, want %q", got.Schema.Status, drift.StatusCurrent)
	}
}

// An unparseable expected_schema is refused rather than ignored. Falling back
// to this binary's own version would answer a question nobody asked and report
// "current" for it — a typo in a monitoring config turning into a green light.
func TestSystemRejectsAnUnparseableExpectedSchema(t *testing.T) {
	for _, q := range []string{"?expected_schema=eighteen", "?expected_schema=-1", "?expected_schema=18.0"} {
		status, raw := systemResponse(t, shippedBuild, 18, 18, q)
		if status != http.StatusBadRequest {
			t.Errorf("/api/v1/system%s = %d, want 400: %s", q, status, raw)
		}
	}
	// And a valid one is honoured, so the rejection above is not the endpoint
	// refusing the parameter outright.
	got := systemDrift(t, shippedBuild, 18, 18, "?expected_schema=25")
	if got.Schema.MigrationsBehind != 7 {
		t.Errorf("migrations_behind = %d, want 7", got.Schema.MigrationsBehind)
	}
}

// The drift check must be impossible to wire up without the thing it compares
// against. A server built with no known schema version would report "unknown"
// on every request forever, and an absent mechanism reading as a clean bill of
// health is exactly the failure #150 exists to encode against.
func TestNewRequiresTheSchemaVersionThisBinaryKnows(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	cfg.HTTP.Auth.Enabled = false

	if _, err := httpapi.New(httpapi.Options{
		Config: cfg, DB: db, Events: eventLog, Logger: slog.New(slog.DiscardHandler),
	}); err == nil {
		t.Fatal("a server was built with no known schema version, so its drift check compares nothing")
	}
}
