package weblogin_test

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/api/weblogin"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/voidbind-go/notify"
	vbweblogin "github.com/rarebit-one/voidbind-go/weblogin"
)

// captureChannel is a fake WakeChannel: it records every ping (and the
// subscription it was aimed at) instead of POSTing to a live ntfy server, so the
// push tests need NO network and NO ntfy deployment. It serves notify.ChannelNtfy
// so a ChannelNtfy subscription routes to it.
type captureChannel struct {
	mu    sync.Mutex
	pings []notify.Ping
	subs  []notify.Subscription
}

func (c *captureChannel) Name() string { return notify.ChannelNtfy }

func (c *captureChannel) Wake(_ context.Context, sub notify.Subscription, ping notify.Ping) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pings = append(c.pings, ping)
	c.subs = append(c.subs, sub)
	return nil
}

func (c *captureChannel) captured() []notify.Ping {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]notify.Ping, len(c.pings))
	copy(out, c.pings)
	return out
}

// pushHarness is the QR harness plus the injected capture channel, so a test can
// drive a real login initiation through the real Handler and inspect what push
// was fanned out.
type pushHarness struct {
	*harness
	ch *captureChannel
}

// newPushHarness stands up the full weblogin.Handler with a fake wake channel and
// a fresh in-memory subscription store — the real broker, the real trust adapter,
// the real /v1/subscriptions registry — so the push assertions run end to end.
func newPushHarness(t *testing.T) *pushHarness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	db, err := sqlite.Open(ctx, sqlite.Options{Path: dir + "/heyarr.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := deviceauth.New(deviceauth.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	ch := &captureChannel{}
	h, err := weblogin.New(weblogin.Options{
		Identities:  store,
		Base:        base,
		WakeChannel: ch,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return &pushHarness{harness: &harness{ts: ts, h: h, store: store}, ch: ch}
}

// subscribe registers a device's ntfy wake endpoint through the real, cert-authed
// POST /v1/subscriptions route — exactly how a phone enrols its wake address.
func (h *pushHarness) subscribe(t *testing.T, cert, endpoint string) {
	t.Helper()
	body := `{"cert":"` + cert + `","channel":"ntfy","endpoint":"` + endpoint + `"}`
	resp, err := h.ts.Client().Post(h.ts.URL+weblogin.SubscriptionsPrefix, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("subscribe = %d (%s), want 200", resp.StatusCode, b)
	}
}

// TestPushWakesSubscribedUserOnLoginInit proves the load-bearing behaviour: an
// enrolled+subscribed user's login initiation (POST /login) fires exactly one
// Enqueue — the capture channel receives one ping for the minted login.
func TestPushWakesSubscribedUserOnLoginInit(t *testing.T) {
	t.Parallel()
	h := newPushHarness(t)
	cert, _ := h.enrolledDevice(t)
	h.subscribe(t, cert, "https://ntfy.example/heyarr-me")

	cr := h.create(t) // POST /login

	got := h.ch.captured()
	if len(got) != 1 {
		t.Fatalf("captured %d pings on login init, want 1", len(got))
	}
	// The pushed tuple is byte-identical to the QR the same login rendered.
	if got[0].Tuple != cr.QR {
		t.Fatalf("ping tuple %q != login QR %q", got[0].Tuple, cr.QR)
	}
}

// TestPushSkipsUnsubscribedUser proves an enrolled-but-unsubscribed user's login
// initiation wakes nobody — no subscription means no ping reaches a channel — and
// it is NOT an error (the browser falls back to the QR the response already held).
func TestPushSkipsUnsubscribedUser(t *testing.T) {
	t.Parallel()
	h := newPushHarness(t)
	_, _ = h.enrolledDevice(t) // pinned, but the device never subscribed a wake endpoint

	cr := h.create(t) // POST /login succeeds and returns a QR
	if cr.QR == "" {
		t.Fatal("login init returned no QR fallback")
	}
	if got := h.ch.captured(); len(got) != 0 {
		t.Fatalf("captured %d pings for an unsubscribed user, want 0", len(got))
	}
}

// TestPushPingIsOpaque is the sovereignty invariant: the pushed ping carries ONLY
// the public (rp, id) tuple — byte-identical to the QR — and no cert, challenge,
// match number, device/user key, or any other secret. A leaked ping tells a wake
// channel only "a login is pending", nothing sovereign.
func TestPushPingIsOpaque(t *testing.T) {
	t.Parallel()
	h := newPushHarness(t)
	cert, _ := h.enrolledDevice(t)
	h.subscribe(t, cert, "https://ntfy.example/heyarr-me")

	cr := h.create(t)

	got := h.ch.captured()
	if len(got) != 1 {
		t.Fatalf("captured %d pings, want 1", len(got))
	}
	tuple := got[0].Tuple

	// It decodes back to exactly (base, id) — the entire opaque content — and
	// byte-matches the QR the browser was shown.
	wantQR, err := vbweblogin.EncodeLogin(base, cr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tuple != wantQR || tuple != cr.QR {
		t.Fatalf("ping tuple %q, want QR %q (== %q)", tuple, cr.QR, wantQR)
	}
	rpBase, id, err := vbweblogin.DecodeLogin(tuple)
	if err != nil {
		t.Fatalf("DecodeLogin(%q): %v", tuple, err)
	}
	if rpBase != base || id != cr.ID {
		t.Fatalf("decoded (%q,%q), want (%q,%q)", rpBase, id, base, cr.ID)
	}

	// It leaks nothing sovereign: no cert body, no device/user key material, no
	// signature, no challenge nonce. Only the RP origin and the opaque login id.
	for _, secret := range []string{cert, "cert", "sig", "signature", "nonce", "challenge"} {
		if strings.Contains(tuple, secret) {
			t.Fatalf("opaque ping leaked %q: %s", secret, tuple)
		}
	}
}

// TestSubscriptionRoutesBehindCertAuth proves the mounted /v1/subscriptions
// surface is gated by the pinned trust set: an enrolled device's cert registers
// (and unregisters) a wake endpoint, while an un-pinned cert is refused 401.
func TestSubscriptionRoutesBehindCertAuth(t *testing.T) {
	t.Parallel()
	h := newPushHarness(t)
	cert, _ := h.enrolledDevice(t)

	// Register: the enrolled device subscribes a wake endpoint → 200.
	body := `{"cert":"` + cert + `","channel":"ntfy","endpoint":"https://ntfy.example/heyarr-me"}`
	resp, err := h.ts.Client().Post(h.ts.URL+weblogin.SubscriptionsPrefix, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("register = %d (%s), want 200", resp.StatusCode, b)
	}
	_ = resp.Body.Close()

	// An un-pinned user's cert is refused — only enrolled devices may subscribe.
	_, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	strangerPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	strangerCert, err := enrolment.SignCert(userPriv, strangerPub, "", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	badBody := `{"cert":"` + strangerCert + `","channel":"ntfy","endpoint":"https://ntfy.example/x"}`
	resp, err = h.ts.Client().Post(h.ts.URL+weblogin.SubscriptionsPrefix, "application/json", strings.NewReader(badBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		_ = resp.Body.Close()
		t.Fatalf("un-pinned register = %d, want 401", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Unsubscribe: the same cert removes this device's subscription → 204.
	req, err := http.NewRequest(http.MethodDelete, h.ts.URL+weblogin.SubscriptionsPrefix, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		_ = resp.Body.Close()
		t.Fatalf("unsubscribe = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// After unsubscribing, a login init wakes nobody — the address book is empty.
	_ = h.create(t)
	if got := h.ch.captured(); len(got) != 0 {
		t.Fatalf("captured %d pings after unsubscribe, want 0", len(got))
	}
}
