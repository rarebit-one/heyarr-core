package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/enrolment"

	"github.com/rarebit-one/heyarr-core/internal/auth"
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
	if _, err := h.identities.EnrolUser(ctx, u.UserID(), "owner", ""); err != nil {
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

// `mnemosyne device revoke` against the personal profile (#914): it used to
// stop at the router's "no route matches /api/v1/identities/devices/...". Alice
// has a laptop and an old phone, both holding a personal space; revoking the
// phone by its device identity id tombstones it and re-keys the space away from
// it, so only the laptop holds the new key. A second device is revoked by its
// key as bare hex, tombstone only.
func TestMnemosyneDeviceRevokeOnThePersonalProfile(t *testing.T) {
	ctx := context.Background()
	h := newAPIHarness(t, withAPIAuth, withWrapAuthorizer, withDeviceCredentials, withPersonalProfile)
	token, err := h.tokens.Create(ctx, "ops-admin", []auth.Scope{auth.ScopeRead, auth.ScopeWrite, auth.ScopeAdmin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	alice := h.enrolUser(t, "alice")

	var created spaceCreateView
	out := h.mustRunMnemosyne("space", "create", "--kind", "personal", "--recovery=false", "--device-dir", alice.dir,
		"--token", token.Secret, "--json")
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("space create --json: %v\n%s", err, out)
	}
	phone := h.enrolAnotherDevice(t, alice, "alice-old-phone")
	h.mustRunMnemosyne("space", "wrap", created.ID, "--all-devices", "--device-dir", alice.dir)
	if !h.wrappedFor(t, created.ID, phone.encKey) {
		t.Fatal("setup: the space is not wrapped for the phone")
	}

	out = h.mustRunMnemosyne("device", "revoke", phone.pinned.ID, "--device-dir", alice.dir, "--token", token.Secret, "--json")
	var view deviceRevokeView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if view.Device.ID != phone.pinned.ID || view.Device.RevokedAt == nil || view.Device.EncryptionKey != phone.encKey {
		t.Fatalf("revoked = %+v", view.Device)
	}
	if len(view.Skipped) != 0 || len(view.Rotated) != 1 || view.Rotated[0].SpaceID != created.ID ||
		view.Rotated[0].KeyEpoch != 1 || len(view.Rotated[0].Revoked) != 1 || view.Rotated[0].Revoked[0] != phone.encKey {
		t.Fatalf("rotated/skipped = %+v / %+v, want the space re-keyed away from the phone", view.Rotated, view.Skipped)
	}
	if h.wrappedFor(t, created.ID, phone.encKey) || !h.wrappedFor(t, created.ID, alice.encKey) {
		t.Fatal("after the revoke the space is still wrapped for the phone, or no longer for the laptop")
	}
	if d, err := h.identities.LookupDevice(ctx, phone.pinned.DeviceKey); err != nil || d.RevokedAt == nil {
		t.Fatalf("the phone is not tombstoned: %+v, %v", d, err)
	}
	// The laptop still reads the space, across the rotation.
	h.mustRunMnemosyne("space", "read", created.ID, "--device-dir", alice.dir, "--token", token.Secret)

	tablet := h.enrolAnotherDevice(t, alice, "alice-old-tablet")
	bare := strings.TrimPrefix(tablet.pinned.DeviceKey, identity.Algorithm+":")
	human := h.mustRunMnemosyne("device", "revoke", bare, "--no-rotate", "--token", token.Secret)
	if !strings.Contains(human, "revoked "+tablet.pinned.DeviceKey) {
		t.Fatalf("human output:\n%s", human)
	}
	if d, err := h.identities.LookupDevice(ctx, tablet.pinned.DeviceKey); err != nil || d.RevokedAt == nil {
		t.Fatalf("the tablet is not tombstoned: %+v, %v", d, err)
	}
}

// wrappedFor reports whether the space's current key is wrapped for recipient
// in the store.
func (h *apiHarness) wrappedFor(t *testing.T, spaceID, recipient string) bool {
	t.Helper()
	keys, err := h.spaces.WrappedKeysFor(context.Background(), spaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Recipient == recipient {
			return true
		}
	}
	return false
}
