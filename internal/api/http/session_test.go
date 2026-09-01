package httpapi_test

import (
	"context"
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

// stubMgmt is a ManagementAuthorizer over a fixed set of authorised device keys —
// the interim follow-management grant (ADR-0061), proven at the middleware seam
// without standing up the catalog-backed grant store.
type stubMgmt map[string]bool

func (m stubMgmt) ManagementAuthorized(_ context.Context, deviceKey string) (bool, error) {
	return m[deviceKey], nil
}

func newSessionHarness(t *testing.T, sessions httpapi.SessionValidator) *httptest.Server {
	t.Helper()
	return newSessionHarnessWithAuth(t, sessions, nil)
}

func newSessionHarnessWithAuth(t *testing.T, sessions httpapi.SessionValidator, mgmt httpapi.ManagementAuthorizer) *httptest.Server {
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
		Config:               cfg,
		DB:                   db,
		Verifier:             verifier,
		SessionValidator:     sessions,
		ManagementAuthorizer: mgmt,
		Events:               eventLog,
		Build:                buildinfo.Info{Version: "test", Commit: "abc123", Date: "2026-08-20T00:00:00Z"},
		SchemaVersion:        1,
		KnownSchemaVersion:   1,
		Mount:                []httpapi.MountFunc{testRoutes}, // GET /probe requires read
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

func postProbe(t *testing.T, ts *httptest.Server, authHeader string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/probe", nil)
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

// A web-login session is read-only by default (ADR-0053): it reads, but a write
// route 403s. This is the TV/shared-surface floor — no grant, no write.
func TestSessionTokenIsReadOnlyByDefault(t *testing.T) {
	t.Parallel()
	const token = "session-token-abc"
	sessions := stubSessions{token: {UserID: "ed25519:aa", DeviceKey: "ed25519:tv"}}
	ts := newSessionHarnessWithAuth(t, sessions, stubMgmt{}) // authorizer wired, but nothing granted

	if code := getProbe(t, ts, "Bearer "+token); code != http.StatusOK {
		t.Fatalf("a session should read, got %d", code)
	}
	if code := postProbe(t, ts, "Bearer "+token); code != http.StatusForbidden {
		t.Fatalf("an ungranted session should 403 on a write route, got %d", code)
	}
}

// A follow-management grant (ADR-0061) on the approving device lifts that
// device's session to write: the SAME session token now passes the write route.
// A different, ungranted approving device stays read-only — the grant is
// per-approving-device, not global.
func TestSessionTokenWritesWithManagementGrant(t *testing.T) {
	t.Parallel()
	const granted, ungranted = "granted-token", "ungranted-token"
	sessions := stubSessions{
		granted:   {UserID: "ed25519:aa", DeviceKey: "ed25519:phone"},
		ungranted: {UserID: "ed25519:aa", DeviceKey: "ed25519:other"},
	}
	ts := newSessionHarnessWithAuth(t, sessions, stubMgmt{"ed25519:phone": true})

	if code := postProbe(t, ts, "Bearer "+granted); code != http.StatusCreated {
		t.Fatalf("a granted device's session should write (201), got %d", code)
	}
	if code := postProbe(t, ts, "Bearer "+ungranted); code != http.StatusForbidden {
		t.Fatalf("an ungranted device's session should stay read-only, got %d", code)
	}
	// The grant lifts to write, not admin: an admin route still 403s.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer "+granted)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a management grant must not confer admin, got %d", resp.StatusCode)
	}
}
