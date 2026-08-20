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
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
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

	logs := &syncBuffer{}
	srv, err := httpapi.New(httpapi.Options{
		Config:        cfg,
		Logger:        slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		DB:            db,
		Verifier:      verifier,
		Build:         buildinfo.Info{Version: "test", Commit: "abc123", Date: "2026-08-20T00:00:00Z"},
		SchemaVersion: 4,
		CASRoot:       casDir,
		Mount:         []httpapi.MountFunc{testRoutes},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{server: srv, store: store, logs: logs, cfg: cfg, casDir: casDir}
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
	r.Get("/boom", func(_ http.ResponseWriter, _ *http.Request) {
		panic("the handler exploded, and the stack must not reach the client")
	})
}

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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
