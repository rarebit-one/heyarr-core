package weblogin_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/enrolment"
	"github.com/rarebit-one/void-which-binds-go/notify"
	vbweblogin "github.com/rarebit-one/void-which-binds-go/weblogin"

	"github.com/rarebit-one/heyarr-core/internal/api/weblogin"
)

// planeBearer is the enqueue bearer the fake plane accepts.
const planeBearer = "test-enqueue-bearer" // #nosec G101 -- a test fixture, not a credential

// planeCall is one request the fake notify plane received.
type planeCall struct {
	Route  string
	Bearer string
	Body   map[string]string
}

// fakePlane stands in for the shared notify plane (void-which-binds-notify): it
// serves the two bearer-gated enqueue routes, records every call, and answers
// with a configurable status and woken count. Heyarr reaches it through the REAL
// notify.EnqueueClient, so these tests prove the wire heyarr sends, not a seam.
type fakePlane struct {
	ts *httptest.Server

	mu     sync.Mutex
	calls  []planeCall
	status int // 0 → 200
	woken  int
	// hold, when set, parks every enqueue until it is closed — a wedged plane.
	hold chan struct{}
}

func newFakePlane(t *testing.T) *fakePlane {
	t.Helper()
	p := &fakePlane{woken: 1}
	mux := http.NewServeMux()
	for _, route := range []string{"/v1/enqueue", "/v1/enqueue-unwrap"} {
		mux.HandleFunc("POST "+route, func(w http.ResponseWriter, r *http.Request) {
			if p.hold != nil {
				<-p.hold
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			p.mu.Lock()
			p.calls = append(p.calls, planeCall{
				Route:  route,
				Bearer: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
				Body:   body,
			})
			status, woken := p.status, p.woken
			p.mu.Unlock()
			if status != 0 && status != http.StatusOK {
				http.Error(w, "nope", status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"woken": woken, "dropped": 0, "token": "rcpt"})
		})
	}
	p.ts = httptest.NewServer(mux)
	t.Cleanup(p.ts.Close)
	return p
}

func (p *fakePlane) setStatus(code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = code
}

func (p *fakePlane) received() []planeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]planeCall(nil), p.calls...)
}

// client is the real EnqueueClient pointed at the fake plane (plain http is
// allowed because httptest binds loopback).
func (p *fakePlane) client() *notify.EnqueueClient {
	return &notify.EnqueueClient{Base: p.ts.URL, Token: planeBearer}
}

// pushHarness is the QR harness wired to a fake notify plane, so a test can drive
// a real login initiation (or unwrap wake) through the real Handler and inspect
// what heyarr asked the plane to do.
type pushHarness struct {
	*harness
	plane *fakePlane
}

func newPushHarness(t *testing.T) *pushHarness {
	t.Helper()
	plane := newFakePlane(t)
	c := plane.client()
	h := newHarnessWith(t, func(o *weblogin.Options) {
		o.LoginWaker = c
		o.UnwrapWaker = c
	})
	return &pushHarness{harness: h, plane: plane}
}

// settle waits for every request the harness's server is still handling. The
// login push runs AFTER the browser's response is flushed, so a test that has
// its QR back must settle before asserting what the plane received. Closing an
// httptest.Server blocks until its outstanding handlers return (and the later
// cleanup Close is a no-op).
// settle closes the test server, then waits for every login wake still in
// flight; wakes run off the request path, so closing the server alone does not
// wait for them.
func (h *pushHarness) settle() {
	h.ts.Close()
	weblogin.WaitLoginWakes()
}

// pinnedUserIDs lists the user ids heyarr has pinned, in store order.
func (h *harness) pinnedUserIDs(t *testing.T) []string {
	t.Helper()
	users, err := h.store.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.PublicKey)
	}
	return ids
}

// prove signs a fresh possession proof for cert with the device key — what a
// client sends beside its cert since void-which-binds-go v0.18 (void-which-binds-go#70).
func prove(t *testing.T, devicePriv ed25519.PrivateKey, cert string) string {
	t.Helper()
	proof, err := enrolment.SignPossession(devicePriv, cert, time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

// TestPushEnqueuesLoginOnInit is the load-bearing behaviour: a login initiation
// (POST /login) asks the plane, with the bearer, to wake the pinned user for
// exactly this login — rp_base is this node's base and login_id the minted id.
func TestPushEnqueuesLoginOnInit(t *testing.T) {
	t.Parallel()
	h := newPushHarness(t)
	_, _ = h.enrolledDevice(t)
	users := h.pinnedUserIDs(t)

	cr := h.create(t)
	h.settle()

	got := h.plane.received()
	if len(got) != 1 {
		t.Fatalf("plane received %d calls on login init, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.Route != "/v1/enqueue" || c.Bearer != planeBearer {
		t.Fatalf("call = %s with bearer %q, want /v1/enqueue with the configured bearer", c.Route, c.Bearer)
	}
	want := map[string]string{"user_id": users[0], "rp_base": base, "login_id": cr.ID}
	if len(c.Body) != len(want) {
		t.Fatalf("enqueue body %v, want exactly %v", c.Body, want)
	}
	for k, v := range want {
		if c.Body[k] != v {
			t.Fatalf("enqueue %s = %q, want %q", k, c.Body[k], v)
		}
	}
}

// TestPushAddressesEveryPinnedUser: a QR login is user-agnostic at initiation,
// so every pinned user is asked for; the plane wakes nobody for a user who never
// subscribed.
func TestPushAddressesEveryPinnedUser(t *testing.T) {
	t.Parallel()
	h := newPushHarness(t)
	_, _ = h.enrolledDevice(t)
	_, _ = h.enrolledDevice(t)

	_ = h.create(t)
	h.settle()

	got := h.plane.received()
	if len(got) != 2 {
		t.Fatalf("plane received %d calls for two pinned users, want 2", len(got))
	}
	if got[0].Body["user_id"] == got[1].Body["user_id"] {
		t.Fatalf("both enqueues addressed %q", got[0].Body["user_id"])
	}
}

// TestPushRequestIsOpaque is the sovereignty invariant, on heyarr's side of the
// wire: the plane is told only the public (user, rp, login id) — no cert, key,
// signature or challenge — and the ping it builds from them, EncodeLogin(rp, id),
// is byte-identical to the QR the browser was shown.
func TestPushRequestIsOpaque(t *testing.T) {
	t.Parallel()
	h := newPushHarness(t)
	cert, _ := h.enrolledDevice(t)

	cr := h.create(t)
	h.settle()

	got := h.plane.received()
	if len(got) != 1 {
		t.Fatalf("plane received %d calls, want 1", len(got))
	}
	body := got[0].Body
	tuple, err := vbweblogin.EncodeLogin(body["rp_base"], body["login_id"])
	if err != nil {
		t.Fatal(err)
	}
	if tuple != cr.QR {
		t.Fatalf("the plane's ping %q != login QR %q", tuple, cr.QR)
	}
	for k, v := range body {
		for _, secret := range []string{cert, "sig", "nonce", "challenge"} {
			if strings.Contains(v, secret) {
				t.Fatalf("enqueue field %s leaked %q", k, secret)
			}
		}
	}
}

// TestPushFailureNeverBlocksLogin: whatever the plane answers — a refused
// bearer, enqueue disabled, rate-limited, a server error, or no plane at all —
// the login initiation still succeeds and still returns its QR.
func TestPushFailureNeverBlocksLogin(t *testing.T) {
	t.Parallel()
	for _, code := range []int{http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			h := newPushHarness(t)
			_, _ = h.enrolledDevice(t)
			h.plane.setStatus(code)
			if cr := h.create(t); cr.QR == "" || cr.ID == "" {
				t.Fatalf("login init with the plane answering %d returned no QR", code)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		t.Parallel()
		h := newPushHarness(t)
		_, _ = h.enrolledDevice(t)
		h.plane.ts.Close()
		if cr := h.create(t); cr.QR == "" {
			t.Fatal("login init with an unreachable plane returned no QR")
		}
	})
}

// TestPushIsNotOnTheQRsCriticalPath: the browser has its QR while the plane is
// still being called — a wedged plane delays nobody's login.
func TestPushIsNotOnTheQRsCriticalPath(t *testing.T) {
	t.Parallel()
	plane := newFakePlane(t)
	plane.hold = make(chan struct{})
	c := plane.client()
	h := &pushHarness{harness: newHarnessWith(t, func(o *weblogin.Options) { o.LoginWaker = c }), plane: plane}
	_, _ = h.enrolledDevice(t)

	// Read the create response to EOF, as the sign-in page's r.json() does: a
	// wake on the request path would hold the body open until the held plane
	// answered, even after the QR chunk was flushed.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.ts.URL+"/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		close(plane.hold) // let the server's Close in cleanup finish
		t.Fatalf("login init body did not complete while the plane was held: %v", err)
	}
	var cr struct {
		QR string `json:"qr"`
	}
	if err := json.Unmarshal(body, &cr); err != nil || cr.QR == "" {
		t.Fatalf("login init returned no QR: %v %q", err, body)
	}
	close(plane.hold)
	h.settle()
	if got := plane.received(); len(got) != 1 {
		t.Fatalf("plane received %d calls once released, want 1", len(got))
	}
}

// TestPushStopsAtARefusedBearer: a refused bearer is the same answer for every
// user, so the fan-out stops at the first rather than spending a request per
// pinned user on a plane that has already said no.
func TestPushStopsAtARefusedBearer(t *testing.T) {
	t.Parallel()
	h := newPushHarness(t)
	_, _ = h.enrolledDevice(t)
	_, _ = h.enrolledDevice(t)
	h.plane.setStatus(http.StatusUnauthorized)

	_ = h.create(t)
	h.settle()

	if got := h.plane.received(); len(got) != 1 {
		t.Fatalf("plane received %d calls after refusing the bearer, want 1", len(got))
	}
}

// TestNoPlaneIsQROnly: a node with no notify plane configured mounts the same
// login, which still returns its QR — push is additive.
func TestNoPlaneIsQROnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, _ = h.enrolledDevice(t)
	if cr := h.create(t); cr.QR == "" {
		t.Fatal("login init without a plane returned no QR")
	}
}

// TestSubscriptionRoutesAreGone: phones subscribe to the shared plane, never to
// heyarr (ADR-0102), so the embedded registry's routes no longer exist — with or
// without a plane configured.
func TestSubscriptionRoutesAreGone(t *testing.T) {
	t.Parallel()
	for name, h := range map[string]*harness{"no plane": newHarness(t), "plane": newPushHarness(t).harness} {
		cert, priv := h.enrolledDevice(t)
		body := `{"cert":"` + cert + `","possession":"` + prove(t, priv, cert) + `","channel":"ntfy","endpoint":"https://ntfy.example/topic"}`
		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			req, err := http.NewRequest(method, h.ts.URL+"/v1/subscriptions", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := h.ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s: %s /v1/subscriptions = %d, want 404", name, method, resp.StatusCode)
			}
		}
	}
}
