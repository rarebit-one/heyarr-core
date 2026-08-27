package device_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
)

// TestGenerateProducesAnEncryptionKey: a generated device carries an X25519
// encryption key alongside its signing key — the wrap target §41 needs — and it
// is a real, usable key (ADR-0049). It is a DIFFERENT key from the signing key.
func TestGenerateProducesAnEncryptionKey(t *testing.T) {
	store, _ := newStore(t)
	dev, err := store.Generate("laptop", false)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	enc := dev.EncryptionKeyString()
	if !strings.HasPrefix(enc, "x25519:") {
		t.Fatalf("encryption key %q is not x25519-prefixed", enc)
	}
	if _, err := encryption.ParsePublicKey(enc); err != nil {
		t.Fatalf("the device encryption key does not parse: %v", err)
	}
	if enc == dev.PublicKeyString() {
		t.Fatal("the encryption key equals the signing key: they must be distinct primitives")
	}
	// The two key files exist, each 0600.
	for _, name := range []string{"device_ed25519.key", "device_x25519.key"} {
		info, err := os.Stat(filepath.Join(store.Dir(), name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %#o, want 0600", name, info.Mode().Perm())
		}
	}
}

// TestEncryptionKeyRoundTripsThroughLoad: the encryption key generate reports is
// the one load reconstructs — deterministic across reads, like the signing key.
func TestEncryptionKeyRoundTripsThroughLoad(t *testing.T) {
	store, _ := newStore(t)
	created, err := store.Generate("laptop", false)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("list returned %d, want 1", len(listed))
	}
	if got, want := listed[0].EncryptionKeyString(), created.EncryptionKeyString(); got != want {
		t.Fatalf("list reported encryption key %s, generate reported %s", got, want)
	}
}

// TestSwappedEncryptionKeyIsCaught: replacing the encryption key file under a
// record that names a different encryption key is refused, not adopted — the same
// guard the signing key has, so a swapped file cannot silently change what the
// device can be wrapped for.
func TestSwappedEncryptionKeyIsCaught(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.Generate("laptop", false); err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Write a DIFFERENT, well-formed x25519 key into the encryption key file,
	// leaving the record's recorded key unchanged.
	other, err := encryption.GenerateKey()
	if err != nil {
		t.Fatalf("generate a stand-in key: %v", err)
	}
	swapped := "heyarr-device-x25519-seed:" + hexOf(other.Bytes()) + "\n"
	if err := os.WriteFile(filepath.Join(store.Dir(), "device_x25519.key"), []byte(swapped), 0o600); err != nil {
		t.Fatalf("swapping the key file: %v", err)
	}

	if _, err := store.List(); !errors.Is(err, device.ErrMalformedKey) {
		t.Fatalf("a swapped encryption key was accepted (err=%v), want ErrMalformedKey", err)
	}
}

// TestRemoveDeletesTheEncryptionKey: removing a device deletes its encryption key
// file too, leaving no orphaned secret-adjacent material behind.
func TestRemoveDeletesTheEncryptionKey(t *testing.T) {
	store, _ := newStore(t)
	dev, err := store.Generate("laptop", false)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := store.Remove(dev.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), "device_x25519.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the encryption key file survived removal (err=%v)", err)
	}
}

// TestPreMilestone9RecordLoads: a device record written before Milestone 9 — no
// encryption_key field, no x25519 key file — still loads, with no encryption key,
// rather than failing. Such a device authenticates as before; it simply is not
// yet a wrap target (ADR-0032: a key lands before it authorises; here, before it
// is wrapped for).
func TestPreMilestone9RecordLoads(t *testing.T) {
	store, _ := newStore(t)
	dev, err := store.Generate("laptop", false)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Simulate a pre-M9 device: drop the encryption key file and strip the
	// encryption_key line from the record, leaving the signing key intact.
	if err := os.Remove(filepath.Join(store.Dir(), "device_x25519.key")); err != nil {
		t.Fatalf("removing the encryption key file: %v", err)
	}
	recPath := filepath.Join(store.Dir(), "device.json")
	raw, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "encryption_key") {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(recPath, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		t.Fatalf("rewriting the record: %v", err)
	}

	listed, err := store.List()
	if err != nil {
		t.Fatalf("a pre-M9 device failed to load: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("list returned %d, want 1", len(listed))
	}
	if got := listed[0].EncryptionKeyString(); got != "" {
		t.Fatalf("a pre-M9 device reported an encryption key %q, want none", got)
	}
	// It still authenticates as the same signing identity.
	if listed[0].PublicKeyString() != dev.PublicKeyString() {
		t.Fatal("a pre-M9 device changed its signing identity on load")
	}
}

// hexOf is a tiny local hex encoder so the swap test can forge a key file without
// importing encoding/hex at the top for one use.
func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

// TestLoadEncryptionKeyMatchesTheRecordedPublicKey: the private encryption key
// LoadEncryptionKey returns is the private half of the public key the device
// records — the key that unwraps a space key sealed for this device (§41,
// ADR-0049). A device with no encryption key (a pre-M9 device) is refused rather
// than handed a zero key.
func TestLoadEncryptionKeyMatchesTheRecordedPublicKey(t *testing.T) {
	store, _ := newStore(t)
	dev, err := store.Generate("laptop", false)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	priv, err := store.LoadEncryptionKey()
	if err != nil {
		t.Fatalf("LoadEncryptionKey: %v", err)
	}
	got := encryption.FormatPublicKey(priv.PublicKey().Bytes())
	if got != dev.EncryptionKeyString() {
		t.Fatalf("loaded key %s, device records %s", got, dev.EncryptionKeyString())
	}

	// With no device at all, there is nothing to load.
	empty, _ := newStore(t)
	if _, err := empty.LoadEncryptionKey(); err == nil {
		t.Fatal("LoadEncryptionKey on a store with no device should fail")
	}
}
