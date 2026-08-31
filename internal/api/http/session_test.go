package httpapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// stubSessions is a SessionValidator over a fixed token→principal map: the http
// layer's contract with the weblogin broker is only "is this token live, and for
// whom", so the middleware behaviour is proven without standing up a real broker
// (whose own end-to-end mint/validate is covered in internal/api/weblogin).
type stubSessions map[string]httpapi.SessionPrincipal

func (s stubSessions) Session(token string) (httpapi.SessionPrincipal, bool) {
	p, ok := s[token]
	return p, ok
}

func newSessionHarness(t *testing.T, sessions httpapi.SessionValidator) *httptest.Server {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()

	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults() // auth enabled
	cfg.DataDir = dir
	cfg.Peer = config.Peer{Name: "test-peer", Site: "test-site"}
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = filepath.Join(shortTempDir(t), "h.sock")

	authStore, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{Store: authStore})
	if err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}

	srv, err := httpapi.New(httpapi.Options{
		Config:             cfg,
		DB:                 db,
		Verifier:           verifier,
		SessionValidator:   sessions,
		Events:             eventLog,
		Build:              buildinfo.Info{Version: "test", Commit: "abc123", Date: "2026-08-20T00:00:00Z"},
		SchemaVersion:      1,
		KnownSchemaVersion: 1,
		Mount:              []httpapi.MountFunc{testRoutes}, // GET /probe requires read
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func getProbe(t *testing.T, ts *httptest.Server, authHeader string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// A live web-login session token, carried as a Bearer credential, authenticates
// through the real middleware chain and reads a read-scoped route.
func TestSessionTokenAuthenticatesAsBearer(t *testing.T) {
	t.Parallel()
	const token = "session-token-abc"
	sessions := stubSessions{token: {UserID: "ed25519:aa", DeviceKey: "ed25519:bb"}}
	ts := newSessionHarness(t, sessions)

	if code := getProbe(t, ts, "Bearer "+token); code != http.StatusOK {
		t.Fatalf("a live session token should read, got %d", code)
	}
	// A token the broker does not know is a 401 — the negative control.
	if code := getProbe(t, ts, "Bearer session-token-unknown"); code != http.StatusUnauthorized {
		t.Fatalf("an unknown session token should be 401, got %d", code)
	}
	// No credential at all is still a 401 (the route is genuinely guarded).
	if code := getProbe(t, ts, ""); code != http.StatusUnauthorized {
		t.Fatalf("no credential should be 401, got %d", code)
	}
}

// With no SessionValidator wired (the loopback/socket-only node that mounts no
// broker), a session token is just an unrecognised bearer value → 401. The
// scheme is opt-in, exactly like the Device scheme.
func TestSessionTokenIgnoredWhenNoValidator(t *testing.T) {
	t.Parallel()
	ts := newSessionHarness(t, nil)
	if code := getProbe(t, ts, "Bearer session-token-abc"); code != http.StatusUnauthorized {
		t.Fatalf("a session token with no validator should be 401, got %d", code)
	}
}
