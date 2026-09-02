package deviceauth_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// A self-enrolment is judged by the rules an authenticated request is judged
// by: every refusal Verify makes on the cert or the proof, SelfEnrol makes too,
// and the one thing it adds — the row — it adds once.
func TestSelfEnrol(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	store, at := f.store, f.clock.Now()

	u, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnrolUser(ctx, u.UserID(), "owner"); err != nil {
		t.Fatal(err)
	}
	_, strangerPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	devicePub, devicePriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := enrolment.SignCert(userPriv, devicePub, "", at, 0)
	if err != nil {
		t.Fatal(err)
	}
	proof := func(priv ed25519.PrivateKey, over string) string {
		t.Helper()
		p, err := enrolment.SignPossession(priv, over, at, 0)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	strangerCert, err := enrolment.SignCert(strangerPriv, devicePub, "", at, 0)
	if err != nil {
		t.Fatal(err)
	}
	expiredCert, err := enrolment.SignCert(userPriv, otherPub, "", at.Add(-48*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	otherCert, err := enrolment.SignCert(userPriv, otherPub, "", at, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Refusals first, so the row does not exist yet when they are tried: a
	// refusal that only holds because the device is already enrolled proves less.
	refused := []struct {
		name  string
		cert  string
		proof string
		want  error
	}{
		{"empty proof", cert, "", deviceauth.ErrMalformedCredential},
		{"unpinned user's cert", strangerCert, proof(devicePriv, strangerCert), deviceauth.ErrUnknownUser},
		{"expired cert", expiredCert, proof(otherPriv, expiredCert), enrolment.ErrExpired},
		{"proof by the wrong key", cert, proof(otherPriv, cert), enrolment.ErrPossessionSignature},
		{"proof over a different cert", cert, proof(devicePriv, otherCert), enrolment.ErrPossessionCert},
		{"garbage cert", "not-a-cert", proof(devicePriv, cert), enrolment.ErrMalformed},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			_, err := store.SelfEnrol(ctx, tc.cert, tc.proof, "phone")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if _, err := store.LookupDevice(ctx, identity.FormatPublicKey(devicePub)); !errors.Is(err, deviceauth.ErrUnknownDevice) {
				t.Fatalf("a refused self-enrolment left a row: %v", err)
			}
		})
	}

	first, err := store.SelfEnrol(ctx, cert, proof(devicePriv, cert), "phone")
	if err != nil || !first.Created {
		t.Fatalf("first self-enrol: %+v err=%v", first, err)
	}
	dev := first.Device
	if dev.DeviceKey != identity.FormatPublicKey(devicePub) || dev.Name != "phone" || first.User.PublicKey != u.UserID() {
		t.Fatalf("enrolled %+v under %+v", dev, first.User)
	}
	// The device now authenticates through the ordinary path, at nothing more
	// than "this user" — scope is the HTTP layer's, and stays the read floor.
	if a, err := store.Verify(ctx, cert+"~"+proof(devicePriv, cert), at); err != nil || a.DeviceKey != dev.DeviceKey {
		t.Fatalf("verify after self-enrol: %+v %v", a, err)
	}

	again, err := store.SelfEnrol(ctx, cert, proof(devicePriv, cert), "renamed")
	if err != nil || again.Created {
		t.Fatalf("second self-enrol: %+v err=%v", again, err)
	}
	if again.Device.ID != dev.ID || again.Device.Name != "phone" {
		t.Fatalf("re-submit returned a different row or renamed it: %+v", again.Device)
	}
	if devices, err := store.ListDevices(ctx, dev.UserID); err != nil || len(devices) != 1 {
		t.Fatalf("devices after re-submit = %d (%v), want 1", len(devices), err)
	}

	// Revocation is final from the device's side of the door.
	if _, err := store.RevokeDevice(ctx, dev.DeviceKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SelfEnrol(ctx, cert, proof(devicePriv, cert), "phone"); !errors.Is(err, deviceauth.ErrDeviceRevoked) {
		t.Fatalf("revoked device re-enrolled itself: %v", err)
	}
}
