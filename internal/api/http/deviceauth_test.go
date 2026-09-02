package httpapi_test

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// This proves the acceptance sentence's first half at the HTTP boundary: a
// device authenticates as a user through the REAL middleware chain
// (authenticate -> RequireScope), presenting only a user-signed cert and a
// possession proof under the Device scheme, with no token issued — and the
// negatives (no user, revoked device) get a 401.

type deviceAuthHarness struct {
	ts    *httptest.Server
	store *deviceauth.Store
}

func newDeviceAuthHarness(t *testing.T) *deviceAuthHarness {
	return newDeviceAuthHarnessWithMgmt(t, nil)
}

// newDeviceAuthHarnessWithMgmt is newDeviceAuthHarness with a ManagementAuthorizer
// wired, so a test can authorise a specific enrolled device key and prove its
// device credential then carries write (ADR-0065).
func newDeviceAuthHarnessWithMgmt(t *testing.T, mgmt httpapi.ManagementAuthorizer) *deviceAuthHarness {
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

	cfg := config.Defaults() // auth is enabled by default
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
	devStore, err := deviceauth.New(deviceauth.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}

	srv, err := httpapi.New(httpapi.Options{
		Config:               cfg,
		DB:                   db,
		Verifier:             verifier,
		DeviceVerifier:       devStore,
		ManagementAuthorizer: mgmt,
		Events:               eventLog,
		Build:                buildinfo.Info{Version: "test", Commit: "abc123", Date: "2026-08-20T00:00:00Z"},
		SchemaVersion:        1,
		KnownSchemaVersion:   1,
		CASRoot:              casDir,
		Mount:                []httpapi.MountFunc{testRoutes}, // GET /probe read, POST write, DELETE admin
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &deviceAuthHarness{ts: ts, store: devStore}
}

// enrolledDevice pins a user and enrols one device, returning the device key and
// a function that mints a fresh Device credential for a moment.
func (h *deviceAuthHarness) enrolledDevice(t *testing.T) (deviceKey string, credential func() string) {
	t.Helper()
	u, userPriv, err := enrolment.GenerateUserIdentity()
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
	ctx := context.Background()
	if _, err := h.store.EnrolUser(ctx, u.UserID(), "alice"); err != nil {
		t.Fatalf("enrol user: %v", err)
	}
	if _, err := h.store.EnrolDevice(ctx, cert, "phone"); err != nil {
		t.Fatalf("enrol device: %v", err)
	}
	return identity.FormatPublicKey(devicePub), func() string {
		proof, err := enrolment.SignPossession(devicePriv, cert, time.Now().UTC(), 0)
		if err != nil {
			t.Fatal(err)
		}
		return cert + "~" + proof
	}
}

func (h *deviceAuthHarness) get(t *testing.T, authHeader string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+"/api/v1/probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestDeviceCredentialAuthenticatesOverHTTP(t *testing.T) {
	t.Parallel()
	h := newDeviceAuthHarness(t)
	_, credential := h.enrolledDevice(t)

	if code := h.get(t, "Device "+credential()); code != http.StatusOK {
		t.Fatalf("an enrolled device should read, got %d", code)
	}
	// No credential at all is a 401 — the positive control that the route is
	// actually guarded.
	if code := h.get(t, ""); code != http.StatusUnauthorized {
		t.Fatalf("no credential should be 401, got %d", code)
	}
}

func TestRevokedDeviceIsRefusedOverHTTP(t *testing.T) {
	t.Parallel()
	h := newDeviceAuthHarness(t)
	deviceKey, credential := h.enrolledDevice(t)

	// Works before revocation.
	if code := h.get(t, "Device "+credential()); code != http.StatusOK {
		t.Fatalf("precondition: enrolled device should read, got %d", code)
	}
	if _, err := h.store.RevokeDevice(context.Background(), deviceKey); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Refused on the very next request — the store is read every request, so
	// revocation is immediate, not restart-scoped.
	if code := h.get(t, "Device "+credential()); code != http.StatusUnauthorized {
		t.Fatalf("a revoked device should be 401, got %d", code)
	}
}

func TestDeviceCredentialForUnenrolledUserIsRefusedOverHTTP(t *testing.T) {
	t.Parallel()
	h := newDeviceAuthHarness(t)

	// A user identity and device that the server has never pinned.
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
	proof, err := enrolment.SignPossession(devicePriv, cert, time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if code := h.get(t, "Device "+cert+"~"+proof); code != http.StatusUnauthorized {
		t.Fatalf("an unenrolled user should be 401, got %d", code)
	}
}

// ADR-0065's subsume: an enrolled device whose key an admin has authorised
// carries WRITE via its own device credential — the durable convergence that
// replaced ADR-0061's session-lift. An enrolled but unauthorised device stays on
// the read floor, and an authorisation never confers admin.
func TestAuthorizedDeviceWritesOverHTTP(t *testing.T) {
	t.Parallel()
	// A mutable authorizer (a map, held by reference) so the device key can be
	// authorised AFTER enrolment mints it.
	mgmt := stubMgmt{}
	h := newDeviceAuthHarnessWithMgmt(t, mgmt)
	deviceKey, credential := h.enrolledDevice(t)

	// Enrolled but not authorised: reads, but a write route 403s (the floor).
	if code := postProbe(t, h.ts, "Device "+credential()); code != http.StatusForbidden {
		t.Fatalf("an unauthorised device must 403 on write, got %d", code)
	}

	// Authorise exactly this device key — its credential now carries write.
	mgmt[deviceKey] = true
	if code := postProbe(t, h.ts, "Device "+credential()); code != http.StatusCreated {
		t.Fatalf("an authorised device should write (201), got %d", code)
	}

	// Write, not admin: an admin route still 403s — the authorisation lifts to
	// write only, exactly as the retired session-lift did.
	req, _ := http.NewRequest(http.MethodDelete, h.ts.URL+"/api/v1/probe", nil)
	req.Header.Set("Authorization", "Device "+credential())
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a device authorisation must not confer admin, got %d", resp.StatusCode)
	}
}

// A device credential presented under the Bearer scheme is not misread as a
// token, and vice versa — the two schemes stay separate.
func TestSchemesDoNotCrossOver(t *testing.T) {
	t.Parallel()
	h := newDeviceAuthHarness(t)
	_, credential := h.enrolledDevice(t)

	if code := h.get(t, "Bearer "+credential()); code != http.StatusUnauthorized {
		t.Fatalf("a device credential under Bearer should be 401, got %d", code)
	}
}
