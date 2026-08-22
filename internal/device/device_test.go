package device_test

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/device"
)

// fixedClock keeps created_at out of the assertions (ADR-0017).
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func newStore(t *testing.T) (*device.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "config", "heyarr", "device")
	store, err := device.NewStore(device.StoreOptions{
		Dir:   dir,
		Clock: fixedClock{t: time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, dir
}

// TestGenerateRoundTripsThroughList asserts the public key, not the exit code:
// a store that generated a key and then listed a different one would pass any
// "it worked" assertion.
func TestGenerateRoundTripsThroughList(t *testing.T) {
	store, _ := newStore(t)

	created, err := store.Generate("laptop", false)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasPrefix(created.PublicKeyString(), "ed25519:") ||
		len(created.PublicKeyString()) != len("ed25519:")+2*ed25519.PublicKeySize {
		t.Fatalf("public key %q is not identity.FormatPublicKey's shape", created.PublicKeyString())
	}

	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("list returned %d devices, want 1", len(listed))
	}
	if got, want := listed[0].PublicKeyString(), created.PublicKeyString(); got != want {
		t.Errorf("list reported public key %s, generate reported %s", got, want)
	}
	if got, want := listed[0].ID, created.ID; got != want {
		t.Errorf("list reported id %s, generate reported %s", got, want)
	}
	if got, want := listed[0].Name, "laptop"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}

	// A second read must be identical, or the key is being regenerated behind
	// the caller's back.
	again, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if again[0].PublicKeyString() != listed[0].PublicKeyString() || again[0].ID != listed[0].ID {
		t.Error("a second list reported a different device")
	}
}

// TestListIsEmptyRatherThanNil keeps `[]` and `null` from being the same answer
// to a JSON client.
func TestListIsEmptyRatherThanNil(t *testing.T) {
	store, _ := newStore(t)
	got, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("an empty listing returned nil, which encodes as null")
	}
	if len(got) != 0 {
		t.Fatalf("an empty store listed %d devices", len(got))
	}
}

// TestThePrivateKeyIsWrittenOwnerOnly is the assertion sabotage (1) must break.
func TestThePrivateKeyIsWrittenOwnerOnly(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.Generate("laptop", false); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(store.KeyPath())
	if err != nil {
		t.Fatalf("the private key is not where the store says it is: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("the private key is mode %#o, want %#o — "+
			"a key another account can read is a key another account can be this device with", got, want)
	}

	dirInfo, err := os.Stat(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dirInfo.Mode().Perm(), os.FileMode(0o700); got != want {
		t.Errorf("the device directory is mode %#o, want %#o", got, want)
	}
}

// TestTheDeviceRecordCarriesTheCaveat asserts the FIELDS, not the prose around
// them. A key that is not yet load-bearing and does not say so is the failure
// this issue exists to prevent.
func TestTheDeviceRecordCarriesTheCaveat(t *testing.T) {
	store, _ := newStore(t)
	dev, err := store.Generate("laptop", false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dev.EnrolmentStatus(), "not_enrolled"; got != want {
		t.Errorf("enrolment status = %q, want %q", got, want)
	}
	if !dev.Unproven() {
		t.Error("a device key that authorises nothing reported unproven = false")
	}
	view := device.NewView(dev)
	if got, want := view.EnrolmentStatus, "not_enrolled"; got != want {
		t.Errorf("view enrolment_status = %q, want %q", got, want)
	}
	if !view.Unproven {
		t.Error("view unproven = false")
	}
	if view.Authorises != device.NotYetAuthorising {
		t.Errorf("view authorises = %q, want the standard caveat", view.Authorises)
	}
}

// TestRefusals gives each refusal its own case. "Invalid input is rejected"
// would pass with three of the four broken.
func TestRefusals(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, store *device.Store)
		act     func(store *device.Store) error
		want    error
	}{
		{
			name: "a world-readable key file",
			arrange: func(t *testing.T, store *device.Store) {
				t.Helper()
				if _, err := store.Generate("laptop", false); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(store.KeyPath(), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			act:  func(store *device.Store) error { _, err := store.List(); return err },
			want: device.ErrKeyPermissions,
		},
		{
			name: "a key file that is not a key",
			arrange: func(t *testing.T, store *device.Store) {
				t.Helper()
				if _, err := store.Generate("laptop", false); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(store.KeyPath(), []byte("-----BEGIN NOTHING-----\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			act:  func(store *device.Store) error { _, err := store.List(); return err },
			want: device.ErrMalformedKey,
		},
		{
			name: "removing a device that does not exist",
			arrange: func(t *testing.T, store *device.Store) {
				t.Helper()
				if _, err := store.Generate("laptop", false); err != nil {
					t.Fatal(err)
				}
			},
			act: func(store *device.Store) error {
				_, err := store.Remove("01920000-0000-7000-8000-000000000000")
				return err
			},
			want: device.ErrUnknownDevice,
		},
		{
			name: "regenerating without --force",
			arrange: func(t *testing.T, store *device.Store) {
				t.Helper()
				if _, err := store.Generate("laptop", false); err != nil {
					t.Fatal(err)
				}
			},
			act:  func(store *device.Store) error { _, err := store.Generate("laptop", false); return err },
			want: device.ErrDeviceExists,
		},
		{
			name:    "showing a device on a machine that has none",
			arrange: func(t *testing.T, _ *device.Store) { t.Helper() },
			act:     func(store *device.Store) error { _, err := store.Get(""); return err },
			want:    device.ErrNoDevice,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := newStore(t)
			tt.arrange(t, store)
			err := tt.act(store)
			if err == nil {
				t.Fatalf("the operation succeeded, want %v", tt.want)
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v — the refusals are distinct because they call for "+
					"different actions from a person", err, tt.want)
			}
		})
	}
}

// TestRemovingTheKeyLeavesNothingBehind: a "removed" that left the key on disk
// would be the worst possible answer.
func TestRemovingTheKeyLeavesNothingBehind(t *testing.T) {
	store, _ := newStore(t)
	dev, err := store.Generate("laptop", false)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := store.Remove(dev.ID)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if removed.ID != dev.ID {
		t.Errorf("remove reported %s, removed %s", removed.ID, dev.ID)
	}
	for _, path := range []string{store.KeyPath(), store.RecordPath()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after remove (stat error %v)", path, err)
		}
	}
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Errorf("list reported %d devices after removing the only one", len(listed))
	}
}

// TestForceReplacesTheKey — and produces a different one, because a --force
// that quietly kept the old key would be worse than one that refused.
func TestForceReplacesTheKey(t *testing.T) {
	store, _ := newStore(t)
	first, err := store.Generate("laptop", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Generate("laptop", true)
	if err != nil {
		t.Fatalf("generate --force: %v", err)
	}
	if second.PublicKeyString() == first.PublicKeyString() {
		t.Error("--force reported success and left the old public key in place")
	}
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].PublicKeyString() != second.PublicKeyString() {
		t.Errorf("after --force the store holds %v, want only %s", listed, second.PublicKeyString())
	}
}

// TestASwappedKeyFileIsCaught: the record and the key are two files, and two
// files can disagree. Adopting either silently would change this device's
// identity without anyone being told.
func TestASwappedKeyFileIsCaught(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.Generate("laptop", false); err != nil {
		t.Fatal(err)
	}
	other, otherDir := newStore(t)
	if _, err := other.Generate("other", false); err != nil {
		t.Fatal(err)
	}
	stolen, err := os.ReadFile(filepath.Join(otherDir, device.KeyFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.KeyPath(), stolen, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); !errors.Is(err, device.ErrMalformedKey) {
		t.Fatalf("a swapped key file produced %v, want %v", err, device.ErrMalformedKey)
	}
}

// TestTheDeviceTypeCannotCarryKeyMaterial scans everything the store hands back
// for the seed. This is the store-level half of the output scan; the CLI test
// scans real captured output.
func TestTheDeviceTypeCannotCarryKeyMaterial(t *testing.T) {
	store, _ := newStore(t)
	dev, err := store.Generate("laptop", false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	seedHex := strings.TrimSpace(string(raw))
	if i := strings.IndexByte(seedHex, ':'); i >= 0 {
		seedHex = seedHex[i+1:]
	}
	seed, err := hex.DecodeString(seedHex)
	if err != nil {
		t.Fatalf("the key file is not the shape this test assumes: %v", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)

	rendered := []string{
		device.NewView(dev).PublicKey,
		device.NewView(dev).KeyPath,
		device.NewView(dev).Authorises,
		dev.PublicKeyString(),
		dev.KeyPath,
		dev.Name,
		dev.ID,
	}
	needles := map[string]string{
		"the seed in hex":        seedHex,
		"the key file verbatim":  strings.TrimSpace(string(raw)),
		"the private key in hex": hex.EncodeToString(priv),
	}
	for _, got := range rendered {
		for what, needle := range needles {
			if needle == "" {
				t.Fatalf("the test's %s needle is empty, so this assertion proves nothing", what)
			}
			if strings.Contains(got, needle) {
				t.Errorf("%s appears in a rendered device field: %q", what, got)
			}
		}
	}
}
