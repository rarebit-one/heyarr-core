package device_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// storeClock is the instant newStore's fixedClock is pinned to. The enrolment
// tests sign certs against it so validity is a fact of the test rather than of
// wall time (ADR-0017).
var storeClock = time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)

// newUser returns a fresh user identity keypair for signing certs.
func newUser(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// TestEnrolTakesTheLabelOff is the ADR-0032 revisit, proved end to end through
// the store: a device holding a valid cert reports enrolled and proven, and the
// same device reports not_enrolled and unproven before and after.
func TestEnrolTakesTheLabelOff(t *testing.T) {
	store, _ := newStore(t)
	dev, err := store.Generate("box", false)
	if err != nil {
		t.Fatal(err)
	}

	// Before enrolment: the label is on.
	before, err := store.Get("")
	if err != nil {
		t.Fatal(err)
	}
	if got := before.EnrolmentStatus(); got != device.EnrolmentNotEnrolled {
		t.Fatalf("before enrolment: status = %q, want %q", got, device.EnrolmentNotEnrolled)
	}
	if !before.Unproven() {
		t.Fatal("before enrolment: Unproven() = false, want true")
	}

	userPub, userPriv := newUser(t)
	cert, err := enrolment.SignCert(userPriv, dev.PublicKey, storeClock, 0)
	if err != nil {
		t.Fatal(err)
	}

	enrolled, err := store.Enrol(cert)
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	// The label comes off on the value Enrol returns AND on a fresh load — the
	// second is what proves it is the cert on disk, not a flag in memory.
	for _, d := range []device.Device{enrolled, mustGet(t, store)} {
		if got := d.EnrolmentStatus(); got != device.EnrolmentEnrolled {
			t.Fatalf("after enrolment: status = %q, want %q", got, device.EnrolmentEnrolled)
		}
		if d.Unproven() {
			t.Fatal("after enrolment: Unproven() = true, want false")
		}
		if got, want := d.EnrolledUser(), identity.FormatPublicKey(userPub); got != want {
			t.Fatalf("after enrolment: EnrolledUser() = %q, want %q", got, want)
		}
		if _, ok := d.EnrolmentCert(); !ok {
			t.Fatal("after enrolment: EnrolmentCert() reports no cert")
		}
	}
}

// TestEnrolRefusesACertForAnotherDevice binds the cert to the key: a cert whose
// device key is not this machine's is refused, because this machine could never
// sign a possession proof for it — enrolling it would write a credential that
// authenticates nobody.
func TestEnrolRefusesACertForAnotherDevice(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.Generate("box", false); err != nil {
		t.Fatal(err)
	}
	otherPub, _ := newUser(t) // a different Ed25519 key, standing in for another device
	_, userPriv := newUser(t)
	cert, err := enrolment.SignCert(userPriv, otherPub, storeClock, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enrol(cert); !errors.Is(err, device.ErrCertNotForDevice) {
		t.Fatalf("Enrol with a cert for another device: err = %v, want ErrCertNotForDevice", err)
	}
	// And nothing was written: the device is still not enrolled.
	if got := mustGet(t, store).EnrolmentStatus(); got != device.EnrolmentNotEnrolled {
		t.Fatalf("after refused enrolment: status = %q, want %q", got, device.EnrolmentNotEnrolled)
	}
}

// TestEnrolRefusesAnExpiredCert refuses a cert already outside its window, and a
// present-but-expired cert file reads as not_enrolled rather than failing the
// load — the honest label for a lapsed authentication.
func TestEnrolRefusesAnExpiredCert(t *testing.T) {
	store, _ := newStore(t)
	dev, err := store.Generate("box", false)
	if err != nil {
		t.Fatal(err)
	}
	_, userPriv := newUser(t)
	// Issued long ago with a short life, so it is expired at the store's clock.
	cert, err := enrolment.SignCert(userPriv, dev.PublicKey, storeClock.Add(-100*24*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enrol(cert); !errors.Is(err, enrolment.ErrExpired) {
		t.Fatalf("Enrol with an expired cert: err = %v, want ErrExpired", err)
	}

	// Write it directly, bypassing Enrol's guard, and confirm load treats it as
	// not enrolled rather than surfacing an error that would break `device list`.
	if err := os.WriteFile(store.CertPath(), []byte(cert+"\n"), device.KeyFileMode); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get("")
	if err != nil {
		t.Fatalf("Get with an expired cert on disk: %v", err)
	}
	if got.EnrolmentStatus() != device.EnrolmentNotEnrolled || !got.Unproven() {
		t.Fatalf("expired cert on disk: status=%q unproven=%v, want not_enrolled/true",
			got.EnrolmentStatus(), got.Unproven())
	}
}

// TestCredentialAuthenticates proves the credential Credential mints is exactly
// what the server splits and verifies: cert + possession, joined by the shared
// separator, each half verifying against the keys it names.
func TestCredentialAuthenticates(t *testing.T) {
	store, _ := newStore(t)
	dev, err := store.Generate("box", false)
	if err != nil {
		t.Fatal(err)
	}
	userPub, userPriv := newUser(t)
	cert, err := enrolment.SignCert(userPriv, dev.PublicKey, storeClock, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enrol(cert); err != nil {
		t.Fatal(err)
	}

	cred, err := store.Credential(storeClock, 0)
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	certPart, proofPart, ok := strings.Cut(cred, enrolment.CredentialSeparator)
	if !ok {
		t.Fatalf("credential %q is not two halves joined by %q", cred, enrolment.CredentialSeparator)
	}
	// The cert half verifies against the user key.
	verified, err := enrolment.VerifyCert(certPart, userPub, storeClock)
	if err != nil {
		t.Fatalf("the credential's cert half does not verify: %v", err)
	}
	if verified.Device != dev.PublicKeyString() {
		t.Fatalf("the cert binds %q, this device is %q", verified.Device, dev.PublicKeyString())
	}
	// The possession half verifies against the device key and is bound to this
	// exact cert — the property that stops a proof being lifted onto another.
	if err := enrolment.VerifyPossession(proofPart, dev.PublicKey, certPart, storeClock); err != nil {
		t.Fatalf("the credential's possession half does not verify: %v", err)
	}
}

// TestCredentialRefusedWhenNotEnrolled: a possession proof without a cert
// authenticates nobody, so Credential refuses rather than minting half of one.
func TestCredentialRefusedWhenNotEnrolled(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.Generate("box", false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Credential(storeClock, 0); !errors.Is(err, device.ErrNotEnrolled) {
		t.Fatalf("Credential on an un-enrolled device: err = %v, want ErrNotEnrolled", err)
	}
}

// TestUnenrolPutsTheLabelBack: forgetting a cert returns the device to
// not_enrolled without touching the key.
func TestUnenrolPutsTheLabelBack(t *testing.T) {
	store, _ := newStore(t)
	dev, err := store.Generate("box", false)
	if err != nil {
		t.Fatal(err)
	}
	_, userPriv := newUser(t)
	cert, err := enrolment.SignCert(userPriv, dev.PublicKey, storeClock, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enrol(cert); err != nil {
		t.Fatal(err)
	}
	unenrolled, err := store.Unenrol()
	if err != nil {
		t.Fatalf("Unenrol: %v", err)
	}
	if unenrolled.EnrolmentStatus() != device.EnrolmentNotEnrolled {
		t.Fatalf("after unenrol: status = %q, want not_enrolled", unenrolled.EnrolmentStatus())
	}
	// The device key survives: the public key is unchanged.
	if unenrolled.PublicKeyString() != dev.PublicKeyString() {
		t.Fatal("Unenrol changed the device key")
	}
}

func mustGet(t *testing.T, store *device.Store) device.Device {
	t.Helper()
	d, err := store.Get("")
	if err != nil {
		t.Fatal(err)
	}
	return d
}
