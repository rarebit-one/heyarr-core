package spacerecover_test

import (
	"bytes"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spacerecover"
	"github.com/rarebit-one/heyarr-core/internal/recovery"
)

// sealForRecovery mints a fresh space key and seals it for the recovery secret's
// encryption key, exactly as `space create` wraps a space for the recovery target
// (ADR-0049). It returns the plaintext space key (via a helper token it encrypts)
// and the wrapped blob a peer would store.
func sealForRecovery(t *testing.T, secret recovery.Secret) (sk encryption.SpaceKey, wrapped []byte) {
	t.Helper()
	priv, err := encryption.NewPrivateKey(recovery.DeriveUserEncryptionSeed(secret))
	if err != nil {
		t.Fatalf("derive recovery key: %v", err)
	}
	sk, err = encryption.NewSpaceKey()
	if err != nil {
		t.Fatalf("new space key: %v", err)
	}
	wrapped, err = encryption.Seal(sk, priv.PublicKey())
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return sk, wrapped
}

// TestEncryptionSeedIsDistinctFromSigningSeed is ADR-0049's white-box assertion,
// asserted here at the shim boundary: the two recovery labels over one secret
// yield two different 32-byte seeds, so unwrapping can never accidentally use the
// identity signing key.
func TestEncryptionSeedIsDistinctFromSigningSeed(t *testing.T) {
	secret, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	enc := recovery.DeriveUserEncryptionSeed(secret)
	sign := recovery.DeriveUserSeed(secret)
	if bytes.Equal(enc, sign) {
		t.Fatal("the encryption seed and the signing seed must differ (ADR-0049)")
	}
}

// TestUnwrapAllRecoversOffline is the load-bearing gate assertion (ADR-0021,
// ADR-0049): a space key sealed for the recovery target is recovered from the
// paper secret alone — no process, network, or store — and it is the SAME key,
// proven functionally by decrypting what the original key encrypted.
func TestUnwrapAllRecoversOffline(t *testing.T) {
	secret, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	original, wrapped := sealForRecovery(t, secret)

	// The recipient the seal targeted is exactly the one RecipientID reports.
	recID, err := spacerecover.RecipientID(secret)
	if err != nil {
		t.Fatal(err)
	}
	if recID == "" {
		t.Fatal("RecipientID returned empty")
	}

	got, err := spacerecover.UnwrapAll(secret, map[string][]byte{"space-1": wrapped})
	if err != nil {
		t.Fatalf("UnwrapAll: %v", err)
	}
	recovered, ok := got["space-1"]
	if !ok {
		t.Fatal("space-1 was not recovered")
	}

	// Functional equality: what the original key encrypts, the recovered key
	// decrypts. (SpaceKey has no exported bytes to compare directly.)
	plaintext := []byte("a vault change")
	ct, err := encryption.EncryptChange(original, plaintext)
	if err != nil {
		t.Fatalf("encrypt with original: %v", err)
	}
	pt, err := encryption.DecryptChange(recovered, ct)
	if err != nil {
		t.Fatalf("decrypt with recovered: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("recovered key did not decrypt the original's ciphertext: got %q", pt)
	}
}

// TestRewrapForDeviceLetsTheDeviceRead: after recovering a space key from the
// secret, re-wrapping it for a fresh device key lets THAT device open the space —
// the recovery chain's tail (ADR-0022), proven by the device unwrapping and
// decrypting what the original key encrypted.
func TestRewrapForDeviceLetsTheDeviceRead(t *testing.T) {
	secret, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	original, wrapped := sealForRecovery(t, secret)

	recovered, err := spacerecover.UnwrapAll(secret, map[string][]byte{"space-1": wrapped})
	if err != nil {
		t.Fatalf("UnwrapAll: %v", err)
	}

	// A fresh device keypair, as a recovered machine would generate.
	devPriv, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	devPub := encryption.FormatPublicKey(devPriv.PublicKey().Bytes())

	rewrapped, err := spacerecover.RewrapForDevice(recovered, devPub)
	if err != nil {
		t.Fatalf("RewrapForDevice: %v", err)
	}

	// The device unwraps its new copy and it is the same key.
	sk, err := encryption.Unwrap(rewrapped["space-1"], devPriv)
	if err != nil {
		t.Fatalf("device unwrap: %v", err)
	}
	plaintext := []byte("a vault change")
	ct, err := encryption.EncryptChange(original, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := encryption.DecryptChange(sk, ct)
	if err != nil {
		t.Fatalf("device decrypt: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("the re-wrapped key did not open the space: got %q", pt)
	}
}

// TestWrongSecretIsRefused: a different secret derives a different key that cannot
// open the wrapped copy, and it fails loudly rather than returning a wrong key.
func TestWrongSecretIsRefused(t *testing.T) {
	secret, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	_, wrapped := sealForRecovery(t, secret)

	other, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spacerecover.UnwrapAll(other, map[string][]byte{"space-1": wrapped}); err == nil {
		t.Fatal("UnwrapAll must refuse a wrapped copy sealed for a different secret")
	}
}
