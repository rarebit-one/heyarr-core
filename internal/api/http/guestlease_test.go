// Guest access leases (ADR-0094): a credential-less caller from inside the
// trusted-net boundary is admitted as a guest backed by a short-lived M7 lease;
// one from outside must enrol; an empty allow-list turns the tier off; and the
// lease is minted with exactly the browse+play+subtitle capabilities, principal
// "guest", and a 1h life. These drive the real handler with a chosen source
// address (httptest lets a request carry any RemoteAddr) and an injected clock,
// so no test sleeps and the expiry is a fact (ADR-0017).
//
//nolint:bodyclose // recorder responses need no closing
package httpapi_test

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/guest"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// leaseClock is the injected clock (ADR-0017) shared by the server and the lease
// store, so a minted lease's expiry is the wall-free sum now+ttl.
type leaseClock struct{ t time.Time }

func (c *leaseClock) Now() time.Time { return c.t }

// guestFixture is a server wired for guest mode, with the lease store held so a
// test can read back the row that was minted.
type guestFixture struct {
	handler http.Handler
	leases  *leases.Store
	clock   *leaseClock
}

// newGuestFixture builds a real server with guest mode on and the given
// trusted-net allow-list. Everything is real — one database, one lease store on
// the injected clock — nothing mocked.
func newGuestFixture(t *testing.T, trustedNets []string) *guestFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	dbPath := filepath.Join(dir, "heyarr.db")
	testdb.WriteMigrated(t, dbPath)
	db, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

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

	clock := &leaseClock{t: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)}
	_, signer, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaseStore, err := leases.New(leases.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: eventLog, Signer: signer, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Peer = config.Peer{Name: "test-peer", Site: "test-site"}
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	cfg.HTTP.Auth.Enabled = true
	cfg.HTTP.Guest.Enabled = true
	cfg.HTTP.Guest.TrustedNets = trustedNets

	srv, err := httpapi.New(httpapi.Options{
		Config:             cfg,
		Logger:             slog.New(slog.DiscardHandler),
		DB:                 db,
		Verifier:           verifier,
		Events:             eventLog,
		GuestLeases:        guest.NewMinter(leaseStore, guest.DefaultTTL),
		KnownSchemaVersion: 4,
		Now:                clock.Now,
		Mount:              []httpapi.MountFunc{testRoutes},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &guestFixture{handler: srv.Handler(), leases: leaseStore, clock: clock}
}

// from issues a request that carries the given source address, and returns the
// recorder. The source is what remoteHost reads and the allow-list is checked
// against.
func (f *guestFixture) from(method, path, remoteAddr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// The estate ranges these tests use are documentation-safe: 192.0.2.0/24 is
// RFC 5737 TEST-NET-1 (a stand-in LAN), and 203.0.113.0/24 is RFC 5737
// TEST-NET-3, used here as the off-estate "raw internet" that must enrol.
const (
	trustedLAN     = "192.0.2.0/24"
	insideSource   = "192.0.2.10:54321"
	outsideSource  = "203.0.113.7:54321"
	insideBareHost = "192.0.2.10"
)

// A credential-less caller from inside the trusted net is admitted as a guest
// and can read the shared library.
func TestGuestAdmittedFromATrustedSource(t *testing.T) {
	f := newGuestFixture(t, []string{trustedLAN})

	rec := f.from(http.MethodGet, "/api/v1/probe", insideSource)
	if rec.Code != http.StatusOK {
		t.Fatalf("guest from a trusted source = %d, want 200", rec.Code)
	}
}

// A caller from OUTSIDE the boundary is not a guest: raw internet must enrol, so
// the request falls through to the ordinary 401 rather than being admitted.
func TestNonTrustedSourceIsNotAGuestAndMustEnrol(t *testing.T) {
	f := newGuestFixture(t, []string{trustedLAN})

	rec := f.from(http.MethodGet, "/api/v1/probe", outsideSource)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("credential-less caller from outside the boundary = %d, want 401 (must enrol)", rec.Code)
	}
	// And nothing was minted for it — an off-estate probe leaves no lease behind.
	all, err := f.leases.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("an off-estate request minted %d leases, want 0", len(all))
	}
}

// An EMPTY allow-list turns the tier off regardless of Enabled: no source
// matches, so even a loopback caller is refused.
func TestEmptyAllowListDisablesTheTier(t *testing.T) {
	f := newGuestFixture(t, nil)

	for _, src := range []string{insideSource, "127.0.0.1:5000"} {
		if rec := f.from(http.MethodGet, "/api/v1/probe", src); rec.Code != http.StatusUnauthorized {
			t.Errorf("guest with an empty allow-list from %s = %d, want 401", src, rec.Code)
		}
	}
}

// The lease a guest is minted with is the exact ADR-0094 shape: principal
// "guest", capabilities browse+play+subtitle in that order, resource keyed by
// the source address, and a 1h life on the injected clock.
func TestGuestLeaseHasTheExactShape(t *testing.T) {
	f := newGuestFixture(t, []string{trustedLAN})

	if rec := f.from(http.MethodGet, "/api/v1/probe", insideSource); rec.Code != http.StatusOK {
		t.Fatalf("guest admission = %d, want 200", rec.Code)
	}

	all, err := f.leases.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("minted %d leases, want exactly 1", len(all))
	}
	l := all[0]
	if l.Principal != guest.Principal {
		t.Errorf("principal = %q, want %q", l.Principal, guest.Principal)
	}
	wantCaps := guest.Capabilities()
	if len(l.Capabilities) != len(wantCaps) {
		t.Fatalf("capabilities = %v, want %v", l.Capabilities, wantCaps)
	}
	for i, c := range wantCaps {
		if l.Capabilities[i] != c {
			t.Errorf("capability[%d] = %q, want %q", i, l.Capabilities[i], c)
		}
	}
	if l.Resource != "guest-library:"+insideBareHost {
		t.Errorf("resource = %q, want it keyed by the source address", l.Resource)
	}
	if got := l.ExpiresAt.Sub(l.IssuedAt); got != guest.DefaultTTL {
		t.Errorf("lease life = %s, want %s (well under grant.MaxTTL)", got, guest.DefaultTTL)
	}
	if want := f.clock.t.Add(guest.DefaultTTL); !l.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %s, want %s (now + ttl on the injected clock)", l.ExpiresAt, want)
	}
}

// A browsing guest is one lease, not one per request: a second request from the
// same source reuses the live lease rather than writing a fresh row.
func TestGuestLeaseIsReusedPerSource(t *testing.T) {
	f := newGuestFixture(t, []string{trustedLAN})

	for range 3 {
		if rec := f.from(http.MethodGet, "/api/v1/probe", insideSource); rec.Code != http.StatusOK {
			t.Fatalf("guest admission = %d, want 200", rec.Code)
		}
	}
	all, err := f.leases.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("three requests from one source minted %d leases, want 1 (reuse)", len(all))
	}
}

// A rejected credential is never downgraded to a guest even from a trusted
// source: a bad token is a 401, not a silent anonymous read.
func TestARejectedCredentialIsNotDowngradedToAGuest(t *testing.T) {
	f := newGuestFixture(t, []string{trustedLAN})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/probe", nil)
	req.RemoteAddr = insideSource
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a bad token from a trusted source = %d, want 401 (never a silent guest)", rec.Code)
	}
}

// Guest mode enabled with no lease issuer is a construction error: the mode IS
// the lease, so a server that could not mint one must not claim to offer it.
func TestGuestEnabledWithoutAnIssuerIsRefused(t *testing.T) {
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	cfg.HTTP.Guest.Enabled = true

	_, err = httpapi.New(httpapi.Options{
		Config: cfg, DB: db, Events: failingEventHead{},
		Logger: slog.New(slog.DiscardHandler), KnownSchemaVersion: 4,
		Verifier: mustVerifier(t, db),
	})
	if err == nil {
		t.Fatal("a server offered guest mode with no lease issuer")
	}
}

// mustVerifier builds a verifier so the guest-issuer check is what fails the
// construction above, not the auth-verifier check.
func mustVerifier(t *testing.T, db *sqlite.DB) *auth.Verifier {
	t.Helper()
	store, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier(auth.VerifierOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
