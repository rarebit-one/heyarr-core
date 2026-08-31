package weblogin_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/api/weblogin"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	vbweblogin "github.com/rarebit-one/voidbind-go/weblogin"
)

// base is the fixed external origin the broker binds every challenge to and
// embeds in the QR. It need not equal the httptest address the device dials —
// the device signs the challenge as SERVED, whatever its audience — so a fixed
// string keeps the QR assertion deterministic.
const base = "http://heyarr.test"

type harness struct {
	ts    *httptest.Server
	h     *weblogin.Handler
	store *deviceauth.Store
}

func newHarness(t *testing.T) *harness {
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
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := deviceauth.New(deviceauth.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	h, err := weblogin.New(weblogin.Options{Identities: store, Base: base})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return &harness{ts: ts, h: h, store: store}
}

// enrolledDevice pins a user and enrols one device, returning the cert token and
// its device signing key — everything needed to approve a login.
func (h *harness) enrolledDevice(t *testing.T) (cert string, devicePriv ed25519.PrivateKey) {
	t.Helper()
	u, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	devicePub, devicePriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err = enrolment.SignCert(userPriv, devicePub, "", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.store.EnrolUser(ctx, u.UserID(), "alice"); err != nil {
		t.Fatalf("enrol user: %v", err)
	}
	if _, err := h.store.EnrolDevice(ctx, cert, "phone"); err != nil {
		t.Fatalf("enrol device: %v", err)
	}
	return cert, devicePriv
}

type createResp struct {
	ID string `json:"id"`
	QR string `json:"qr"`
}

type pollResp struct {
	Status string `json:"status"`
	Token  string `json:"token"`
	User   string `json:"user"`
}

func (h *harness) create(t *testing.T) createResp {
	t.Helper()
	resp, err := h.ts.Client().Post(h.ts.URL+"/login", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /login = %d", resp.StatusCode)
	}
	var cr createResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatal(err)
	}
	return cr
}

func (h *harness) poll(t *testing.T, id string) pollResp {
	t.Helper()
	resp, err := h.ts.Client().Get(h.ts.URL + "/login/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login/%s = %d", id, resp.StatusCode)
	}
	var pr pollResp
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatal(err)
	}
	return pr
}

// approve drives the device half: fetch the challenge as served, sign it with
// the device key, and POST the approval. Returns the approve status code.
func (h *harness) approve(t *testing.T, id, cert string, devicePriv ed25519.PrivateKey) int {
	t.Helper()
	ctx := context.Background()
	ch, err := vbweblogin.FetchChallenge(ctx, h.ts.Client(), h.ts.URL, id)
	if err != nil {
		t.Fatalf("fetch challenge: %v", err)
	}
	a, err := vbweblogin.SignAssertion(ch, cert, devicePriv)
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	body, _ := json.Marshal(a)
	resp, err := h.ts.Client().Post(h.ts.URL+"/login/"+id+"/approve", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// The whole point: a browser goes from unauthenticated to holding a session
// token an enrolled device approved, and that token validates through the same
// seam the /api/v1 guard uses.
func TestQRLoginMintsAValidatableToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cert, devicePriv := h.enrolledDevice(t)

	cr := h.create(t)

	// The QR is exactly the opaque (rp, id) tuple over the configured base —
	// nothing device- or secret-shaped, and reproducible.
	wantQR, err := vbweblogin.EncodeLogin(base, cr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cr.QR != wantQR {
		t.Fatalf("QR = %q, want %q", cr.QR, wantQR)
	}

	// Pending before any approval.
	if pr := h.poll(t, cr.ID); pr.Status != "pending" {
		t.Fatalf("status before approval = %q, want pending", pr.Status)
	}

	if code := h.approve(t, cr.ID, cert, devicePriv); code != http.StatusNoContent {
		t.Fatalf("approve = %d, want 204", code)
	}

	pr := h.poll(t, cr.ID)
	if pr.Status != "approved" || pr.Token == "" {
		t.Fatalf("after approval status=%q token=%q, want approved + a token", pr.Status, pr.Token)
	}

	// The minted token validates through the SessionValidator seam — the credential
	// a browser now carries as Bearer on heyarr's authenticated routes.
	princ, ok := h.h.Sessions().Session(pr.Token)
	if !ok {
		t.Fatal("the minted token did not validate through Sessions()")
	}
	if princ.UserID == "" || princ.DeviceKey == "" {
		t.Fatalf("session principal is empty: %+v", princ)
	}
	// A bogus token does not validate.
	if _, ok := h.h.Sessions().Session("not-a-real-token"); ok {
		t.Fatal("a bogus token validated")
	}
}

// An approval from a user this node never pinned is refused, and the login stays
// pending so an honest device could still approve it — enrol before trust.
func TestUnpinnedUserApprovalIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// A user + device the store has never seen.
	_, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	devicePub, devicePriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := enrolment.SignCert(userPriv, devicePub, "", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}

	cr := h.create(t)
	if code := h.approve(t, cr.ID, cert, devicePriv); code != http.StatusUnauthorized {
		t.Fatalf("unpinned approval = %d, want 401", code)
	}
	if pr := h.poll(t, cr.ID); pr.Status != "pending" {
		t.Fatalf("a refused approval left status=%q, want it still pending", pr.Status)
	}
}

// The /signin page is served for a browser that has no other way in.
func TestSigninPageServed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	resp, err := h.ts.Client().Get(h.ts.URL + "/signin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /signin = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Voidbind") {
		t.Fatal("the signin page does not mention Voidbind")
	}
}

// New refuses to stand up without the two things a login cannot work without.
func TestNewRequiresStoreAndBase(t *testing.T) {
	t.Parallel()
	if _, err := weblogin.New(weblogin.Options{Base: base}); err == nil {
		t.Fatal("New without an identity store should fail")
	}
	// A store with no base is refused too (a login the device cannot dial back).
	h := newHarness(t)
	if _, err := weblogin.New(weblogin.Options{Identities: h.store, Base: "  "}); err == nil {
		t.Fatal("New without a base origin should fail")
	}
}
