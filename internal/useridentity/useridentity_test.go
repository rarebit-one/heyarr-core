package useridentity_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/useridentity"
)

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
	created, err := store.Generate("me", false)
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
	if _, err := store.Generate("me", false); err != nil {
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
	if _, err := store.Generate("me", false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Generate("me", false); !errors.Is(err, useridentity.ErrIdentityExists) {
		t.Fatalf("second generate: err = %v, want ErrIdentityExists", err)
	}
	// --force replaces it: the key changes.
	first, _ := store.Get()
	forced, err := store.Generate("me", true)
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
	id, err := store.Generate("me", false)
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
