package client_test

// Device-authenticated client, end to end against the REAL verifier (#329).
//
// This is the authoritative proof of the issue's substance: the real
// internal/client, minting a fresh possession proof per request through its
// device transport, authenticates to a peer whose handler runs the real
// internal/deviceauth.Store.Verify — the same verifier the HTTP middleware runs
// — and reads content. The peer's 401 shape (the opaque body and the
// `WWW-Authenticate: Device error="…"` clock hint) mirrors internal/api/http/
// auth.go exactly, so the client is exercised against the refusals it will meet.
//
// The load-bearing case is the injected clock advanced PAST PossessionTTL: the
// client and the verifier read the SAME clock, so a second request minted after
// the advance still succeeds (fresh mint) while a credential captured before it
// 401s `possession_expired`. That is the proof a stale printed string cannot
// give, and it is why it lives here rather than in the two-process shell demo,
// where a real wall clock cannot be advanced two minutes without sleeping two
// minutes (recorded pending in scripts/claims.list, the #387 pattern).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// testClock is one clock shared by the client (minting) and the peer
// (verifying), so advancing it advances time for both — the only way "a request
// two minutes later still works" is a genuine test rather than a two-minute wait.
type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time { return c.t }
func (c *testClock) add(d time.Duration) {
	c.t = c.t.Add(d)
}

var epoch = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

// peer is a minimal HTTP peer that authenticates the Device scheme with the real
// deviceauth verifier and serves one protected resource. serverSkew offsets the
// verifier's clock from the shared clock, to drive the not_yet_valid/expired
// asymmetry a slept device meets.
type peer struct {
	store      *deviceauth.Store
	clock      *testClock
	serverSkew time.Duration
}

func (p *peer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = client.DeviceScheme + " "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		p.reject(w, "")
		return
	}
	cred := strings.TrimSpace(h[len(prefix):])
	now := p.clock.Now().Add(p.serverSkew)
	auth, err := p.store.Verify(r.Context(), cred, nil, now)
	if err != nil {
		p.reject(w, clockHint(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": auth.PrincipalName})
}

// reject mirrors internal/api/http/auth.go: an opaque 401 problem document, plus
// the one recoverable fact — a clock skew — disclosed as a Device challenge.
func (p *peer) reject(w http.ResponseWriter, hint string) {
	if hint != "" {
		w.Header().Set("WWW-Authenticate", client.DeviceScheme+` error="`+hint+`"`)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"title":  "Unauthorized",
		"detail": "the presented credential was rejected",
	})
}

// clockHint is the peer's clock-window disclosure, matching auth.go's
// clockWindowHint over the possession/cert refusals.
func clockHint(err error) string {
	switch {
	case errors.Is(err, enrolment.ErrPossessionExpired), errors.Is(err, enrolment.ErrExpired):
		return "expired"
	case errors.Is(err, enrolment.ErrPossessionNotYet), errors.Is(err, enrolment.ErrNotYetValid):
		return "not_yet_valid"
	default:
		return ""
	}
}

func newPeer(t *testing.T, clock *testClock) *peer {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "peer.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	store, err := deviceauth.New(deviceauth.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: log, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &peer{store: store, clock: clock}
}

// enrolledDevice creates a real on-disk device store, generates a device,
// vouches for it with a fresh user identity, and returns the store (the client's
// Credentialer) alongside the user key and cert. pin controls whether the peer
// pins the user and enrols the device — an unpinned pair is the unpinned-user
// refusal.
func enrolledDevice(t *testing.T, p *peer, clock *testClock, pin bool) (*device.Store, string) {
	t.Helper()
	store, err := device.NewStore(device.StoreOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := store.Generate("phone", false)
	if err != nil {
		t.Fatal(err)
	}
	user, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := enrolment.SignCert(userPriv, dev.PublicKey, dev.EncryptionKeyString(), clock.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enrol(cert); err != nil {
		t.Fatal(err)
	}
	if pin {
		ctx := context.Background()
		if _, err := p.store.EnrolUser(ctx, user.UserID(), "alice", ""); err != nil {
			t.Fatalf("pin user: %v", err)
		}
		if _, err := p.store.EnrolDevice(ctx, cert, "phone"); err != nil {
			t.Fatalf("enrol device: %v", err)
		}
	}
	return store, user.UserID()
}

// get issues one authenticated GET and decodes the JSON body.
func get(ctx context.Context, c *client.Client) (map[string]any, error) {
	var out map[string]any
	err := c.Get(ctx, "/ping", nil, &out)
	return out, err
}

func TestDeviceClientAuthenticatesAndReadsThenStillReadsPastTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &testClock{t: epoch}
	p := newPeer(t, clock)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	store, _ := enrolledDevice(t, p, clock, true)

	c, err := client.New(client.Options{Addr: srv.URL, Device: store, Clock: clock.Now})
	if err != nil {
		t.Fatal(err)
	}

	// First: the real client authenticates as a device and reads content.
	got, err := get(ctx, c)
	if err != nil {
		t.Fatalf("the device client was refused on a valid first request: %v", err)
	}
	if got["user"] != "alice" {
		t.Fatalf("read the wrong principal: %+v", got)
	}

	// Capture what a client that CACHED the credential would keep re-presenting.
	stale, err := store.Credential(clock.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}

	// Advance the shared clock well past the two-minute possession TTL.
	clock.add(enrolment.PossessionTTL + time.Minute)

	// The real client still reads: it minted a FRESH proof for this request.
	if _, err := get(ctx, c); err != nil {
		t.Fatalf("the device client was refused minutes later — it should have re-minted: %v", err)
	}

	// The cached string, replayed raw, is now 401 possession_expired — which is
	// exactly the failure per-request minting avoids.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/ping", nil)
	req.Header.Set("Authorization", client.DeviceScheme+" "+stale)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a stale cached credential should have been rejected, got %d", resp.StatusCode)
	}
	if hint := resp.Header.Get("WWW-Authenticate"); !strings.Contains(hint, "expired") {
		t.Fatalf("a stale credential should read as expired, got %q", hint)
	}
}

func TestDeviceClientReMintsGracefullyOnClockSkew(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := map[string]time.Duration{
		// The peer's clock behind this machine's: the first proof is issued in the
		// peer's future and reads not_yet_valid until the client nudges it back.
		"peer behind (not_yet_valid)": -70 * time.Second,
		// The peer's clock ahead: the first proof is already expired against it
		// until the client nudges its issue time forward.
		"peer ahead (expired)": 150 * time.Second,
	}
	for name, skew := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			clock := &testClock{t: epoch}
			p := newPeer(t, clock)
			p.serverSkew = skew
			srv := httptest.NewServer(p)
			t.Cleanup(srv.Close)
			store, _ := enrolledDevice(t, p, clock, true)

			c, err := client.New(client.Options{Addr: srv.URL, Device: store, Clock: clock.Now})
			if err != nil {
				t.Fatal(err)
			}
			got, err := get(ctx, c)
			if err != nil {
				t.Fatalf("the client should re-mint across a moderate clock skew, not fail: %v", err)
			}
			if got["user"] != "alice" {
				t.Fatalf("read the wrong principal: %+v", got)
			}
		})
	}
}

func TestDeviceClientSurfacesCleanErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("revoked device", func(t *testing.T) {
		t.Parallel()
		clock := &testClock{t: epoch}
		p := newPeer(t, clock)
		srv := httptest.NewServer(p)
		t.Cleanup(srv.Close)
		store, _ := enrolledDevice(t, p, clock, true)

		dev, err := store.Get("")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.store.RevokeDevice(ctx, dev.PublicKeyString()); err != nil {
			t.Fatalf("revoke: %v", err)
		}

		c, err := client.New(client.Options{Addr: srv.URL, Device: store, Clock: clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		assertCleanRejection(t, c, ctx)
	})

	t.Run("unpinned user", func(t *testing.T) {
		t.Parallel()
		clock := &testClock{t: epoch}
		p := newPeer(t, clock)
		srv := httptest.NewServer(p)
		t.Cleanup(srv.Close)
		store, _ := enrolledDevice(t, p, clock, false) // never pinned at the peer

		c, err := client.New(client.Options{Addr: srv.URL, Device: store, Clock: clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		assertCleanRejection(t, c, ctx)
	})
}

// assertCleanRejection asserts a refusal surfaces as the device-scheme sentence,
// not the peer's opaque body or a bare status number.
func assertCleanRejection(t *testing.T, c *client.Client, ctx context.Context) {
	t.Helper()
	_, err := get(ctx, c)
	if err == nil {
		t.Fatal("a refused device should not have read content")
	}
	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want a *client.Error, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", apiErr.Status)
	}
	if apiErr.DeviceAdvice == "" {
		t.Fatalf("a device-auth 401 should carry actionable advice, got none: %v", apiErr)
	}
	msg := apiErr.Error()
	if !strings.Contains(msg, "device") || !strings.Contains(msg, "pinned") {
		t.Fatalf("the rejection should name the device and the pinning gate, got %q", msg)
	}
	if strings.Contains(msg, "401") {
		t.Fatalf("the client should not surface a raw 401, got %q", msg)
	}
}
