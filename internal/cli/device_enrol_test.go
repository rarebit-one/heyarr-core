package cli

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rarebit-one/void-which-binds-go/device"
	"github.com/rarebit-one/void-which-binds-go/enrolment"

	"github.com/rarebit-one/heyarr-core/internal/api/enrol"
	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// enrolPeer is a peer serving only POST /enrol, through the real handler and a
// real device-identity store, so the CLI is tested as a client of the route the
// phones use rather than of the test's idea of it.
func enrolPeer(t *testing.T) (*httptest.Server, *deviceauth.Store) {
	t.Helper()
	db := testdb.Migrated(t)
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	identities, err := deviceauth.New(deviceauth.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	h, err := enrol.New(enrol.Options{Identities: identities, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, identities
}

// pairedDeviceDir is what pairing leaves on a machine: a software device whose
// store holds an op the user signed for it.
func pairedDeviceDir(t *testing.T, admit func(device.Device) string) (dir, devKey string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "device")
	store, err := openDeviceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := store.Generate("sync-daemon", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enrol(admit(dev)); err != nil {
		t.Fatal(err)
	}
	return dir, dev.PublicKeyString()
}

func TestDeviceEnrolSelfEnrolsAtThePeer(t *testing.T) {
	srv, identities := enrolPeer(t)
	user, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	dir, devKey := pairedDeviceDir(t, func(dev device.Device) string {
		t.Helper()
		cert, err := enrolment.SignCert(userPriv, dev.PublicKey, dev.EncryptionKeyString(), time.Now().UTC(), 0)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	})
	ctx := context.Background()

	// An unpinned user is the peer's opaque refusal, surfaced as a failure.
	if _, _, err := run(t, ctx, "device", "--device-dir", dir, "enrol", "--peer", srv.URL); err == nil ||
		!strings.Contains(err.Error(), "admin user pin") {
		t.Errorf("enrolling under an unpinned user = %v, want the 401 explained", err)
	}

	if _, err := identities.EnrolUser(ctx, user.UserID(), "owner", rekeyOldRecovery); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, ctx, "device", "--device-dir", dir, "enrol", "--peer", srv.URL, "--json")
	if err != nil {
		t.Fatalf("device enrol: %v", err)
	}
	var got client.EnrolledDevice
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decoding %s: %v", out, err)
	}
	if got.DeviceKey != devKey || got.Name != "sync-daemon" || got.RecoveryEncryptionKey != rekeyOldRecovery {
		t.Errorf("enrolled = %+v, want device %s named sync-daemon carrying the recovery key", got, devKey)
	}
	if got.EncryptionKey == "" {
		t.Error("the enrolment did not bind the device's encryption key, so nothing could be wrapped for it")
	}

	// --name overrides, and a second enrolment of the same device is not an error.
	out, _, err = run(t, ctx, "device", "--device-dir", dir, "enrol", "--peer", srv.URL, "--name", "vault-sync")
	if err != nil {
		t.Fatalf("re-enrolling: %v", err)
	}
	for _, want := range []string{"enrolled at", devKey, "admitted by"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDeviceEnrolRefusals(t *testing.T) {
	ctx := context.Background()
	if _, _, err := run(t, ctx, "device", "--device-dir", t.TempDir(), "enrol"); err == nil ||
		!strings.Contains(err.Error(), "--peer") {
		t.Errorf("enrol without --peer = %v, want a refusal naming the flag", err)
	}
	// A machine with a device that was never paired has nothing to present.
	dir := filepath.Join(t.TempDir(), "device")
	store, err := openDeviceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Generate("lonely", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, ctx, "device", "--device-dir", dir, "enrol", "--peer", "http://127.0.0.1:1"); err == nil ||
		!strings.Contains(err.Error(), "cannot present a credential") {
		t.Errorf("enrolling an unpaired device = %v, want a credential refusal before any request", err)
	}
}

// The client's copy of the route must be the server's.
func TestClientEnrolPathMatchesServer(t *testing.T) {
	if client.EnrolPath != httpapi.EnrolPath {
		t.Errorf("client.EnrolPath = %q, server serves %q", client.EnrolPath, httpapi.EnrolPath)
	}
}
