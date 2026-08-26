package enrolment

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

var testNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// a user identity and a device keypair.
func actors(t *testing.T) (userPriv ed25519.PrivateKey, user UserIdentity, devicePub ed25519.PublicKey) {
	t.Helper()
	u, upriv, err := GenerateUserIdentity()
	if err != nil {
		t.Fatalf("user identity: %v", err)
	}
	dpub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("device key: %v", err)
	}
	return upriv, u, dpub
}

func TestRoundTripAuthenticatesTheDevice(t *testing.T) {
	t.Parallel()
	userPriv, user, devicePub := actors(t)

	tok, err := SignCert(userPriv, devicePub, testNow, 0)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	c, err := VerifyCert(tok, user.PublicKey, testNow)
	if err != nil {
		t.Fatalf("a valid cert was refused: %v (%s)", err, ReasonForCert(err))
	}
	if c.User != user.UserID() {
		t.Fatalf("cert authority = %q, want %q", c.User, user.UserID())
	}
	if c.Device != identity.FormatPublicKey(devicePub) {
		t.Fatalf("cert authenticates %q, want the device key %q", c.Device, identity.FormatPublicKey(devicePub))
	}
	// Default lifetime is 90 days.
	if got := c.ExpiresAt.Sub(c.IssuedAt); got != CertLifetime {
		t.Fatalf("default lifetime = %s, want %s", got, CertLifetime)
	}
}

// The same device key, enrolled by user A, authenticates as A against a SECOND
// verifier that has also pinned A — the "either peer, same user" property (§40).
func TestSameCertAuthenticatesAtEitherPeer(t *testing.T) {
	t.Parallel()
	userPriv, user, devicePub := actors(t)
	tok, err := SignCert(userPriv, devicePub, testNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Two independent verifiers, each holding only the pinned public key.
	for _, name := range []string{"peer-a", "peer-b"} {
		if _, err := VerifyCert(tok, user.PublicKey, testNow); err != nil {
			t.Fatalf("%s refused a valid cert: %v", name, err)
		}
	}
}

// A verifier that has pinned NO user (or the wrong one) does not honour a cert —
// enrol before trust (ADR-0032).
func TestUnenrolledUserIsRefused(t *testing.T) {
	t.Parallel()
	userPriv, _, devicePub := actors(t)
	tok, err := SignCert(userPriv, devicePub, testNow, 0)
	if err != nil {
		t.Fatal(err)
	}

	// No pinned key at all.
	if _, err := VerifyCert(tok, nil, testNow); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("nil pinned key: want ErrUnknownUser, got %v", err)
	}
	// A DIFFERENT user's key pinned: the cert names someone this verifier has
	// not enrolled.
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if _, err := VerifyCert(tok, otherPub, testNow); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("wrong pinned user: want ErrUnknownUser, got %v", err)
	}
	if r := ReasonForCert(errors.New("x")); r != ReasonMalformed {
		t.Fatalf("unmapped error should be malformed, got %q", r)
	}
}

// A cert that names the pinned user but is signed by a DIFFERENT key fails the
// signature — the user field selects the key to check, the signature is the
// authority. This is the door that must stay shut: forging enrolment.
func TestCertSignedByAnotherKeyFailsSignature(t *testing.T) {
	t.Parallel()
	userPriv, user, devicePub := actors(t)
	tok, err := SignCert(userPriv, devicePub, testNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Re-sign the SAME payload with an impostor key, keeping the honest user id.
	body, _, _ := strings.Cut(tok, ".")
	raw, err := decode(body)
	if err != nil {
		t.Fatal(err)
	}
	_, impostor, _ := ed25519.GenerateKey(nil)
	forged := encode(raw) + "." + encode(ed25519.Sign(impostor, raw))

	if _, err := VerifyCert(forged, user.PublicKey, testNow); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a cert signed by an impostor should fail signature, got %v", err)
	}
}

// Tampering any signed field is caught: the device key it authenticates, the
// user, or the window. None may yield an honoured cert.
func TestTamperedCertIsNeverHonoured(t *testing.T) {
	t.Parallel()
	userPriv, user, devicePub := actors(t)
	tok, err := SignCert(userPriv, devicePub, testNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	body, sig, _ := strings.Cut(tok, ".")
	raw, err := decode(body)
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		mutated := append([]byte(nil), raw...)
		mutated[i] ^= 0x01
		if _, err := VerifyCert(encode(mutated)+"."+sig, user.PublicKey, testNow); err == nil {
			t.Fatalf("byte %d: a tampered cert was honoured", i)
		}
	}
}

func TestMalformedCerts(t *testing.T) {
	t.Parallel()
	_, user, _ := actors(t)
	for _, tok := range []string{"", "no-dot", "not-base64!.nope", "."} {
		if _, err := VerifyCert(tok, user.PublicKey, testNow); !errors.Is(err, ErrMalformed) {
			t.Fatalf("token %q: want ErrMalformed, got %v", tok, err)
		}
	}
}

// Expiry is refused by the verifier's own clock, proven by advancing it, and the
// skew margin only ever shortens the honoured window (fails toward refusal).
func TestExpiryAndSkewFailTowardRefusal(t *testing.T) {
	t.Parallel()
	userPriv, user, devicePub := actors(t)
	// A short-lived cert so the boundary is easy to name.
	tok, err := SignCert(userPriv, devicePub, testNow, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// At the true expiry: already refused (window shortened by the margin).
	if _, err := VerifyCert(tok, user.PublicKey, testNow.Add(time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("at true expiry a cert should be refused, got %v", err)
	}
	// A margin before expiry: the first refused instant.
	if _, err := VerifyCert(tok, user.PublicKey, testNow.Add(time.Hour-SkewMargin)); !errors.Is(err, ErrExpired) {
		t.Fatalf("a margin before expiry should refuse, got %v", err)
	}
	// Comfortably inside: honoured.
	if _, err := VerifyCert(tok, user.PublicKey, testNow.Add(time.Hour-SkewMargin-time.Minute)); err != nil {
		t.Fatalf("inside the shortened window a cert must be honoured, got %v", err)
	}
	// Before issue beyond the margin: not yet valid.
	if _, err := VerifyCert(tok, user.PublicKey, testNow.Add(-2*SkewMargin)); !errors.Is(err, ErrNotYetValid) {
		t.Fatalf("before issue a cert should be not_yet_valid, got %v", err)
	}
}

func TestSignCertRefusesIncompleteCerts(t *testing.T) {
	t.Parallel()
	userPriv, _, devicePub := actors(t)
	if _, err := SignCert(userPriv, nil, testNow, 0); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("no device key: want ErrIncomplete, got %v", err)
	}
	if _, err := SignCert(userPriv, devicePub, time.Time{}, 0); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("no issued-at: want ErrIncomplete, got %v", err)
	}
	if _, err := SignCert(nil, devicePub, testNow, 0); err == nil {
		t.Fatal("a nil signing key should be refused")
	}
}

// The cert's device field round-trips as a parseable key, so the transport can
// go on to challenge that exact key for possession.
func TestAuthenticatedDeviceKeyIsUsable(t *testing.T) {
	t.Parallel()
	userPriv, user, devicePub := actors(t)
	tok, _ := SignCert(userPriv, devicePub, testNow, 0)
	c, err := VerifyCert(tok, user.PublicKey, testNow)
	if err != nil {
		t.Fatal(err)
	}
	got, err := identity.ParsePublicKey(c.Device)
	if err != nil {
		t.Fatalf("authenticated device key does not parse: %v", err)
	}
	if !got.Equal(devicePub) {
		t.Fatalf("authenticated device key != the enrolled one")
	}
}
