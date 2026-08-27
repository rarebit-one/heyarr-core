package useridentity_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/recovery"
	"github.com/rarebit-one/heyarr-core/internal/useridentity"
)

// TestRecoverReconstructsTheSameIdentity is the load-bearing recovery property
// (ADR-0022, §79): the secret Generate displays reconstructs the SAME identity —
// same public key — on a machine with no surviving identity, so a recovered user
// re-issues device certs that verify against the already-pinned key and nothing
// is re-pinned. It is asserted on the public key, not on an exit code: a recover
// that produced a DIFFERENT key would pass any "it worked" check and fail every
// pinned peer.
func TestRecoverReconstructsTheSameIdentity(t *testing.T) {
	original := newStore(t)
	created, secret, err := original.Generate("me", false)
	if err != nil {
		t.Fatal(err)
	}
	if secret.String() == "" {
		t.Fatal("Generate did not return a displayable recovery secret")
	}

	// A DIFFERENT machine — a fresh store with no identity — recovers from the
	// secret alone. No server, no surviving key.
	fresh := newStore(t)
	recovered, err := fresh.RecoverFromSecret(secret, "recovered", false)
	if err != nil {
		t.Fatalf("RecoverFromSecret: %v", err)
	}
	if recovered.UserID() != created.UserID() {
		t.Fatalf("recovery produced %q, the original was %q — a different identity",
			recovered.UserID(), created.UserID())
	}

	// And it is really usable: a cert the recovered store signs verifies against
	// the ORIGINAL public key, so peers that pinned the original honour it.
	devPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := fresh.SignCert(devPub, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enrolment.VerifyCert(cert, created.PublicKey, clockAt); err != nil {
		t.Fatalf("a recovered identity's cert did not verify against the original key: %v", err)
	}
}

// TestRecoverFromWrongSecretFailsLoud: a mistyped secret is rejected at parse
// time and never reconstructs a garbage key (ADR-0022's SLIP-39-over-Shamir
// concern, applied to the base secret). The wiring surfaces the loud failure
// rather than deriving a plausible-but-wrong identity.
func TestRecoverFromWrongSecretFailsLoud(t *testing.T) {
	// A corrupted secret: a valid one with one character changed.
	good, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	enc := []byte(good.String())
	// Flip a character in the data part (after the "heyarr1" separator) to a
	// different, valid charset symbol so the checksum — not the charset — is
	// what rejects it.
	i := len(enc) - 3
	if enc[i] == 'q' {
		enc[i] = 'p'
	} else {
		enc[i] = 'q'
	}
	if _, err := recovery.ParseSecret(string(enc)); !errors.Is(err, recovery.ErrCorruptSecret) {
		t.Fatalf("a corrupted secret should be ErrCorruptSecret, got %v", err)
	}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

var clockAt = time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *useridentity.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "config", "heyarr", "identity")
	store, err := useridentity.NewStore(useridentity.StoreOptions{Dir: dir, Clock: fixedClock{t: clockAt}})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestGenerateRoundTrips asserts the public key survives a generate/get, not the
// exit code: a store that generated one key and read back another would pass any
// "it worked" check.
func TestGenerateRoundTrips(t *testing.T) {
	store := newStore(t)
	created, _, err := store.Generate("me", false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get()
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID() != created.UserID() {
		t.Fatalf("Get returned %q, Generate made %q", got.UserID(), created.UserID())
	}
	if got.Name != "me" {
		t.Fatalf("name = %q, want me", got.Name)
	}
}

// TestPrivateKeyIsOwnerOnly: the seed is written 0600, and a key readable by
// more than its owner is refused on read.
func TestPrivateKeyIsOwnerOnly(t *testing.T) {
	store := newStore(t)
	if _, _, err := store.Generate("me", false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != useridentity.KeyFileMode {
		t.Fatalf("key mode = %#o, want %#o", perm, useridentity.KeyFileMode)
	}
	if err := os.Chmod(store.KeyPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(); !errors.Is(err, useridentity.ErrKeyPermissions) {
		t.Fatalf("Get on a group-readable key: err = %v, want ErrKeyPermissions", err)
	}
}

// TestGenerateRefusesToClobber: a second generate refuses without --force,
// because replacing the identity invalidates every device it enrolled.
func TestGenerateRefusesToClobber(t *testing.T) {
	store := newStore(t)
	if _, _, err := store.Generate("me", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Generate("me", false); !errors.Is(err, useridentity.ErrIdentityExists) {
		t.Fatalf("second generate: err = %v, want ErrIdentityExists", err)
	}
	// --force replaces it: the key changes.
	first, _ := store.Get()
	forced, _, err := store.Generate("me", true)
	if err != nil {
		t.Fatalf("forced generate: %v", err)
	}
	if forced.UserID() == first.UserID() {
		t.Fatal("--force did not change the identity")
	}
}

// TestSignCertProducesAVerifiableBinding: the cert the store signs verifies
// against the identity's public key and binds the device key it was given —
// which is the whole contract SignCert exists for.
func TestSignCertProducesAVerifiableBinding(t *testing.T) {
	store := newStore(t)
	id, _, err := store.Generate("me", false)
	if err != nil {
		t.Fatal(err)
	}
	// A device to enrol.
	devDir := filepath.Join(t.TempDir(), "device")
	devStore, err := device.NewStore(device.StoreOptions{Dir: devDir, Clock: fixedClock{t: clockAt}})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := devStore.Generate("box", false)
	if err != nil {
		t.Fatal(err)
	}

	cert, err := store.SignCert(dev.PublicKey, "", 0)
	if err != nil {
		t.Fatalf("SignCert: %v", err)
	}
	verified, err := enrolment.VerifyCert(cert, id.PublicKey, clockAt)
	if err != nil {
		t.Fatalf("the signed cert does not verify against the identity key: %v", err)
	}
	if verified.User != id.UserID() {
		t.Fatalf("cert user = %q, want %q", verified.User, id.UserID())
	}
	if verified.Device != identity.FormatPublicKey(dev.PublicKey) {
		t.Fatalf("cert device = %q, want %q", verified.Device, identity.FormatPublicKey(dev.PublicKey))
	}
}

// TestSignCertNeedsAnIdentity: signing before generate is ErrNoIdentity, not a
// panic on an absent key.
func TestSignCertNeedsAnIdentity(t *testing.T) {
	store := newStore(t)
	devicePub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SignCert(devicePub, "", 0); !errors.Is(err, useridentity.ErrNoIdentity) {
		t.Fatalf("SignCert with no identity: err = %v, want ErrNoIdentity", err)
	}
}

// expectedRecoveryEncKey derives the recovery x25519 public key id the way the
// store must, from the secret alone — the offline derivation #360 depends on.
func expectedRecoveryEncKey(t *testing.T, secret recovery.Secret) string {
	t.Helper()
	priv, err := encryption.NewPrivateKey(recovery.DeriveUserEncryptionSeed(secret))
	if err != nil {
		t.Fatalf("deriving the expected recovery encryption key: %v", err)
	}
	return encryption.FormatPublicKey(priv.PublicKey().Bytes())
}

// TestGeneratePersistsRecoveryEncryptionKey: a generated identity carries a
// recovery x25519 key derived from the secret (#360), it is the default space
// recipient, and it round-trips through the on-disk record unchanged.
func TestGeneratePersistsRecoveryEncryptionKey(t *testing.T) {
	store := newStore(t)
	id, secret, err := store.Generate("me", false)
	if err != nil {
		t.Fatal(err)
	}
	want := expectedRecoveryEncKey(t, secret)
	if id.EncryptionKey != want {
		t.Fatalf("generated recovery key = %q, want %q (derived from the secret)", id.EncryptionKey, want)
	}
	if !strings.HasPrefix(id.EncryptionKey, "x25519:") {
		t.Fatalf("recovery key %q is not an x25519 recipient id", id.EncryptionKey)
	}
	// Read back from disk: the record persisted it, load returns the same.
	reloaded, err := store.Get()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.EncryptionKey != want {
		t.Fatalf("reloaded recovery key = %q, want %q — it did not survive the record round-trip", reloaded.EncryptionKey, want)
	}
}

// TestRecoverRegeneratesTheSameRecoveryEncryptionKey is the load-bearing #360
// property: the recovery key is reproducible from the secret alone, so a space
// wrapped for it stays openable after every device is lost. A recovery on a fresh
// machine must yield the SAME x25519 key, or the wrapped copy is unrecoverable —
// exactly the failure recovery exists to prevent. Asserted on the key, not an
// exit code.
func TestRecoverRegeneratesTheSameRecoveryEncryptionKey(t *testing.T) {
	original := newStore(t)
	created, secret, err := original.Generate("me", false)
	if err != nil {
		t.Fatal(err)
	}
	if created.EncryptionKey == "" {
		t.Fatal("Generate persisted no recovery encryption key")
	}

	fresh := newStore(t) // a different machine, no surviving identity
	recovered, err := fresh.RecoverFromSecret(secret, "recovered", false)
	if err != nil {
		t.Fatalf("RecoverFromSecret: %v", err)
	}
	if recovered.EncryptionKey != created.EncryptionKey {
		t.Fatalf("recovery produced recovery key %q, the original was %q — a space wrapped for the original would be unrecoverable",
			recovered.EncryptionKey, created.EncryptionKey)
	}
}

// TestPreRecoveryRecordRoundTripsWithoutEncryptionKey: an identity record written
// before #360 (no encryption_key field) loads without error and reads back as "no
// recovery key", so the omitempty field is genuinely backward-compatible.
func TestPreRecoveryRecordRoundTripsWithoutEncryptionKey(t *testing.T) {
	store := newStore(t)
	if _, _, err := store.Generate("me", false); err != nil {
		t.Fatal(err)
	}
	// Rewrite the record with the encryption_key stripped, as a pre-#360 file.
	raw, err := os.ReadFile(store.RecordPath())
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	delete(rec, "encryption_key")
	stripped, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.RecordPath(), append(stripped, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := store.Get()
	if err != nil {
		t.Fatalf("a pre-#360 record failed to load: %v", err)
	}
	if id.EncryptionKey != "" {
		t.Fatalf("a record with no encryption_key read back %q, want empty", id.EncryptionKey)
	}
}
