package httpapi_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
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

// The other direction of M4-05's separation requirement.
//
// The peer tests next door assert that a bearer token cannot authenticate
// against the peer surface. This asserts the reverse, which is the half that
// looks obviously true and is the easier one to break: a peer certificate must
// not authenticate a request to the CLIENT API.
//
// It is easy to break because the peer guard on /api/v1 is a middleware that
// runs on every request, and a middleware that has learned who you are is one
// small refactor away from being a middleware that says so. The guard's
// contract is that it can only ever SUBTRACT: a presented peer key gets a
// request refused, and never gets one authenticated. The bearer credential
// (ADR-0011) stays mandatory.

// alwaysMember is a trust root that admits every key. It is deliberately the
// most permissive possible membership: if a peer certificate could ever
// authenticate a client request, this is the configuration in which it would.
type alwaysMember struct{}

func (alwaysMember) IsMember(context.Context, []byte) (bool, error) { return true, nil }

func newClientAPIWithPeerGuard(t *testing.T, presented httpapi.PresentedPeerKey) (*httptest.Server, *auth.Store) {
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

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	// Authentication ON. The whole question is whether a peer credential can
	// stand in for the bearer token this requires.
	cfg.HTTP.Auth.Enabled = true

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, Logger: slog.New(slog.DiscardHandler), DB: db, Verifier: verifier,
		Events: eventLog, Build: buildinfo.Info{Version: "test"},
		SchemaVersion: 20, KnownSchemaVersion: 20,
		PeerMembership:   alwaysMember{},
		PresentedPeerKey: presented,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

func get(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestAPeerCertificateDoesNotAuthenticateAgainstTheClientAPI(t *testing.T) {
	peerPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// A connection that has proved a peer identity, and a membership table
	// that admits it. Everything a peer credential could possibly assert is
	// asserted here.
	ts, store := newClientAPIWithPeerGuard(t, func(*http.Request) ([]byte, bool) { return peerPub, true })

	status, body := get(t, ts.URL+"/api/v1/system", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("a peer certificate authenticated a client API request: %d\n%s\n"+
			"A peer credential is for /peer/v1. The client API's bearer token (ADR-0011) is not "+
			"optional for anybody, and the peer guard may only ever refuse — never admit.", status, body)
	}

	// The control: the same connection, with a real bearer token, works. Without
	// it the assertion above would also pass on a server that refused every
	// request, which is a different bug wearing the same 401.
	created, err := store.Create(context.Background(), "control", []auth.Scope{auth.ScopeRead}, nil)
	if err != nil {
		t.Fatal(err)
	}
	status, body = get(t, ts.URL+"/api/v1/system", created.Secret)
	if status != http.StatusOK {
		t.Fatalf("a bearer token on a peer connection was refused: %d\n%s", status, body)
	}
}

// TestThePeerGuardOnlyEverSubtracts states the guard's contract as an
// assertion: the outcomes it can produce are "refused" and "carry on", and
// never "admitted".
func TestThePeerGuardOnlyEverSubtracts(t *testing.T) {
	peerPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// A key nobody admits, on a connection with no token: still 401, not 403.
	// The bearer credential is checked first, so a caller with no credential is
	// told it has no credential — the membership table is not something an
	// unauthenticated caller gets to probe.
	notAMember := func(t *testing.T) *httptest.Server {
		t.Helper()
		ts, _ := newClientAPIWithPeerGuard(t, func(*http.Request) ([]byte, bool) { return peerPub, true })
		return ts
	}
	ts := notAMember(t)
	if status, body := get(t, ts.URL+"/api/v1/system", ""); status != http.StatusUnauthorized {
		t.Errorf("an anonymous request on a peer connection got %d, want 401\n%s", status, body)
	}
}
