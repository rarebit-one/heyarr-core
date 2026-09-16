package yubikey_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/yubikey"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
)

// TestRoundTripAgainstTheCard seals a space key to the card's cv25519 public
// point and unwraps it by driving the on-card PSO:DECIPHER — the ADR-0098
// YubiKey backend end to end. It needs a real OpenPGP card with a cv25519
// encryption key and its User PIN, so it is gated on HEYARR_YUBIKEY_HW=1; the
// PIN comes from HEYARR_YUBIKEY_PIN (default 123456, the freshly-provisioned
// card's default). scdaemon must own the card (the usual gpg setup).
func TestRoundTripAgainstTheCard(t *testing.T) {
	if os.Getenv("HEYARR_YUBIKEY_HW") != "1" {
		t.Skip("set HEYARR_YUBIKEY_HW=1 to run the on-card round-trip (needs a cv25519 OpenPGP card)")
	}
	pin := os.Getenv("HEYARR_YUBIKEY_PIN")
	if pin == "" {
		pin = "123456"
	}

	u, err := yubikey.New("", func() (string, error) { return pin, nil })
	if err != nil {
		t.Fatalf("open card: %v", err)
	}

	pub, err := encryption.ParsePublicKey(encryption.FormatPublicKey(u.PublicKey()))
	if err != nil {
		t.Fatalf("card public key: %v", err)
	}
	space, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := encryption.Seal(space, pub)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// A canary under the ORIGINAL key confirms the card-unwrapped key is identical
	// (SpaceKey is opaque, so we compare by function, not by bytes).
	canary := []byte("heyarr yubikey on-card round-trip")
	ct, err := encryption.EncryptChange(space, canary)
	if err != nil {
		t.Fatal(err)
	}

	got, err := u.Unwrap(wrapped)
	if err != nil {
		t.Fatalf("unwrap on card: %v", err)
	}
	if got.IsZero() {
		t.Fatal("unwrapped a zero space key")
	}
	pt, err := encryption.DecryptChange(got, ct)
	if err != nil {
		t.Fatalf("decrypt canary with card-unwrapped key: %v", err)
	}
	if !bytes.Equal(pt, canary) {
		t.Fatalf("canary = %q, want %q", pt, canary)
	}

	// A blob wrapped to a DIFFERENT key must not unwrap on this card.
	otherPriv, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	otherWrapped, err := encryption.Seal(space, otherPriv.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Unwrap(otherWrapped); err == nil {
		t.Fatal("unwrap of a blob wrapped to another key: want error, got nil")
	}
}
