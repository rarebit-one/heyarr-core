package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// `device revoke` is the admin's tombstone plus the ADR-0049 re-key for that
// one device. With no space wrapped for the device there is nothing to
// re-key, and the command says so — an empty list, never null — and the
// device is refused from then on. A second revoke is idempotent.
func TestDeviceRevokeTombstonesAndReportsRotation(t *testing.T) {
	h := newAPIHarness(t, withAPIAuth)
	ctx := context.Background()
	admin, err := h.tokens.Create(ctx, "admin", []auth.Scope{auth.ScopeAdmin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.identities.EnrolUser(ctx, u.UserID(), "owner"); err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	cert, err := enrolment.SignCert(userPriv, pub, "x25519:"+strings.Repeat("ab", 32), h.clock.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.identities.EnrolDevice(ctx, cert, "phone"); err != nil {
		t.Fatal(err)
	}
	deviceKey := identity.FormatPublicKey(pub)
	deviceDir := t.TempDir()
	generateDevice(t, deviceDir)

	out := h.mustRun("device", "revoke", deviceKey, "--device-dir", deviceDir, "--token", admin.Secret, "--json")
	var view struct {
		Device struct {
			DeviceKey     string     `json:"device_key"`
			EncryptionKey string     `json:"encryption_key"`
			AdmittedBy    string     `json:"admitted_by"`
			RevokedAt     *time.Time `json:"revoked_at"`
		} `json:"device"`
		Rotated []json.RawMessage `json:"rotated"`
		Skipped []json.RawMessage `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if view.Device.DeviceKey != deviceKey || view.Device.RevokedAt == nil || view.Device.AdmittedBy != u.UserID() {
		t.Fatalf("revoked = %+v", view.Device)
	}
	if view.Rotated == nil || view.Skipped == nil || len(view.Rotated) != 0 || len(view.Skipped) != 0 {
		t.Fatalf("rotated/skipped = %v / %v, want [] / []", view.Rotated, view.Skipped)
	}
	d, err := h.identities.LookupDevice(ctx, deviceKey)
	if err != nil || d.RevokedAt == nil {
		t.Fatalf("device after revoke = %+v, %v", d, err)
	}
	// Idempotent; the human rendering names what it did.
	human := h.mustRun("device", "revoke", deviceKey, "--device-dir", deviceDir, "--token", admin.Secret)
	if !strings.Contains(human, "revoked "+deviceKey) || !strings.Contains(human, "re-keyed 0 space(s)") {
		t.Fatalf("human output:\n%s", human)
	}
	// --no-rotate leaves the spaces alone and says so.
	if out := h.mustRun("device", "revoke", deviceKey, "--device-dir", deviceDir, "--token", admin.Secret, "--no-rotate"); !strings.Contains(out, "--no-rotate") {
		t.Fatalf("no-rotate output:\n%s", out)
	}
	// An unknown device is an error, not a silent no-op.
	if _, _, err := h.run("device", "revoke", "ed25519:"+strings.Repeat("00", 32), "--token", admin.Secret); err == nil {
		t.Fatal("revoking an unknown device succeeded")
	}
}
