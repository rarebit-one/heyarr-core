package devicekeys

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/custody"
	"github.com/rarebit-one/void-which-binds-go/custody/sealedfile"
	"github.com/rarebit-one/void-which-binds-go/device"
	"github.com/rarebit-one/void-which-binds-go/encryption"
)

// The sealed file's Argon2id floor (256 MiB, time 3) is fixed for callers
// outside the library, and costs seconds under -race, so only
// TestSealedFileDeviceAsksOnceAndNeverReadsASeed derives at all: once to seal
// and once to unlock.

func newStore(t *testing.T, dir string) *device.Store {
	t.Helper()
	s, err := device.NewStore(device.StoreOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// keysProvisioner hands out keys it was given, so a custody RECORD can be made
// with no sealed file and no key derivation.
type keysProvisioner struct{ keys custody.Keys }

func (p keysProvisioner) Provision(custody.Spec) (custody.Keys, error) { return p.keys, nil }

func softwareKeys(t *testing.T) custody.Keys {
	t.Helper()
	_, sign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return custody.SoftwareKeys(sign, enc)
}

func countingPIN(pass string) (custody.PINFunc, *atomic.Int32) {
	var n atomic.Int32
	return func() (string, error) {
		n.Add(1)
		return pass, nil
	}, &n
}

// A missing device and a software device open exactly as device.NewStore did,
// and Holder gives the software key in process (client.KeyUnwrapper's type).
func TestOpenSoftwareDeviceIsUnchanged(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "device")
	ds, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ds.Get(""); !errors.Is(err, device.ErrNoDevice) {
		t.Fatalf("an empty directory: %v, want ErrNoDevice", err)
	}
	dev, err := newStore(t, dir).Generate("laptop", false)
	if err != nil {
		t.Fatal(err)
	}
	if ds, err = Open(Options{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := ds.LoadEncryptionKey(); err != nil {
		t.Fatalf("a software device's key: %v", err)
	}
	h, err := Holder(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.(*custody.KeyUnwrapper); !ok || h.RecipientID() != dev.EncryptionKeyString() {
		t.Fatalf("Holder = %T %s, want a KeyUnwrapper for %s", h, h.RecipientID(), dev.EncryptionKeyString())
	}
	if _, err := os.Stat(SealedPath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a software device has a sealed file: %v", err)
	}
}

// A custody record whose sealed file is gone is an error at every entry point,
// never a fallback to a seed file.
func TestCustodyRecordWithoutItsSealedFile(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "device")
	if _, err := newStore(t, dir).GenerateInto("laptop", false, keysProvisioner{softwareKeys(t)}); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: dir}); !errors.Is(err, ErrNoSealedFile) {
		t.Fatalf("Open: %v, want ErrNoSealedFile", err)
	}
	if _, err := Holder(Options{Dir: dir}); !errors.Is(err, ErrNoSealedFile) {
		t.Fatalf("Holder: %v, want ErrNoSealedFile", err)
	}
	if err := Unlock(Options{Dir: dir}, DefaultUnlockTTL); !errors.Is(err, ErrNoSealedFile) {
		t.Fatalf("Unlock: %v, want ErrNoSealedFile", err)
	}
	// Unlock on a software device, or none, does nothing.
	if err := Unlock(Options{Dir: filepath.Join(t.TempDir(), "none")}, DefaultUnlockTTL); err != nil {
		t.Fatalf("Unlock with no device: %v", err)
	}
}

// TestSealedFileDeviceAsksOnceAndNeverReadsASeed: a device generated into a
// sealed file has no seed files; opening it asks for nothing; the first private
// operation asks once and every later one — across stores opened on the same
// file — does not; it never hands out a raw key; and a removed sealed file is
// forgotten.
func TestSealedFileDeviceAsksOnceAndNeverReadsASeed(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "device")
	pin, asked := countingPIN("correct horse battery staple")
	dev, err := newStore(t, dir).GenerateInto("laptop", false, &sealedfile.Provisioner{Path: SealedPath(dir), PIN: pin})
	if err != nil {
		t.Fatal(err)
	}
	if got := asked.Load(); got != 1 {
		t.Fatalf("generating asked %d times, want 1", got)
	}
	for _, name := range []string{device.KeyFileName, device.EncryptionKeyFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s exists (%v)", name, err)
		}
	}

	ds, err := Open(Options{Dir: dir, PIN: pin})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ds.LoadSigningKey(); !errors.Is(err, device.ErrKeyInCustody) {
		t.Fatalf("LoadSigningKey: %v, want ErrKeyInCustody", err)
	}
	if _, err := ds.LoadEncryptionKey(); !errors.Is(err, device.ErrKeyInCustody) {
		t.Fatalf("LoadEncryptionKey: %v, want ErrKeyInCustody", err)
	}
	signer, err := ds.Signer()
	if err != nil {
		t.Fatal(err)
	}
	h, err := Holder(Options{Dir: dir, PIN: pin})
	if err != nil {
		t.Fatal(err)
	}
	if h.RecipientID() != dev.EncryptionKeyString() {
		t.Fatalf("holder %s, record %s", h.RecipientID(), dev.EncryptionKeyString())
	}
	if got := asked.Load(); got != 1 {
		t.Fatalf("opening asked for the passphrase (%d asks)", got)
	}

	msg := []byte("a body to sign")
	sig, err := signer.Sign(rand.Reader, msg, crypto.Hash(0))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(dev.PublicKey, msg, sig) {
		t.Fatal("the custody signature does not verify under the device key")
	}
	sk, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := encryption.ParsePublicKey(h.RecipientID())
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := encryption.Seal(sk, recipient)
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.Unwrap(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encryption.EncryptChange(sk, msg)
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := encryption.DecryptChange(got, ct); err != nil || string(pt) != string(msg) {
		t.Fatalf("the sealed holder opened a different space key: %v", err)
	}
	if n := asked.Load(); n != 2 {
		t.Fatalf("a signature and an unwrap asked %d times in all, want 2 (seal, then one unlock)", n)
	}

	// A refused hash asks for nothing.
	if _, err := signer.Sign(rand.Reader, msg, crypto.SHA512); err == nil {
		t.Fatal("a pre-hashed signature was produced")
	}
	if n := asked.Load(); n != 2 {
		t.Fatalf("a refused signature asked for the passphrase (%d asks)", n)
	}

	if err := RemoveSealed(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Dir: dir, PIN: pin}); !errors.Is(err, ErrNoSealedFile) {
		t.Fatalf("after RemoveSealed: %v, want ErrNoSealedFile", err)
	}
	if err := RemoveSealed(dir); err != nil {
		t.Fatalf("removing a missing sealed file: %v", err)
	}
}

func TestPassphraseSources(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "pass")
	if err := os.WriteFile(file, []byte(" three words here \r\nsecond line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		file  string
		stdin string
		want  string
	}{
		{"a file keeps inner and edge spaces, drops the line ending", file, "", " three words here "},
		{"stdin", "-", "from stdin\n", "from stdin"},
		{"stdin with no newline", "-", "no newline", "no newline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadPassphraseFile(tc.file, strings.NewReader(tc.stdin))
			if err != nil || got != tc.want {
				t.Fatalf("ReadPassphraseFile = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := ReadPassphraseFile(filepath.Join(dir, "missing"), nil); err == nil {
		t.Fatal("a missing passphrase file was read")
	}

	// The flag wins over the environment; the environment serves both a new
	// passphrase and an existing device's.
	envFile := filepath.Join(dir, "env-pass")
	if err := os.WriteFile(envFile, []byte("from the environment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PassphraseFileEnvVar, envFile)
	if got, err := NewPassphrase(file, nil)(); err != nil || got != " three words here " {
		t.Fatalf("NewPassphrase(flag) = %q, %v", got, err)
	}
	if got, err := NewPassphrase("", nil)(); err != nil || got != "from the environment" {
		t.Fatalf("NewPassphrase(env) = %q, %v", got, err)
	}
	if got, err := DefaultPIN("device.sealed")(); err != nil || got != "from the environment" {
		t.Fatalf("DefaultPIN(env) = %q, %v", got, err)
	}
}

// A new passphrase needs MinPassphraseLen code points, counted as characters,
// not bytes; an existing device's passphrase (DefaultPIN) is not checked.
func TestNewPassphraseMinimumLength(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct {
		name, pass string
		ok         bool
	}{
		{"eleven characters", "elevenchars", false},
		{"twelve characters", "twelve chars", true},
		{"twelve code points in more bytes", "pässwörd-ñ-é", true},
		{"eleven code points in twelve bytes or more", "pässwörd-ñé", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewPassphrase("-", strings.NewReader(tc.pass+"\n"))()
			if tc.ok && (err != nil || got != tc.pass) {
				t.Fatalf("NewPassphrase(%q) = %q, %v", tc.pass, got, err)
			}
			if !tc.ok && !errors.Is(err, ErrPassphraseTooShort) {
				t.Fatalf("NewPassphrase(%q): %v, want ErrPassphraseTooShort", tc.pass, err)
			}
		})
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPassphrase(short, nil)(); !errors.Is(err, ErrPassphraseTooShort) {
		t.Fatalf("a short passphrase file: %v, want ErrPassphraseTooShort", err)
	}
	// Generating with it writes nothing: the passphrase is refused before the
	// seal, so there is neither a record nor a sealed file.
	devDir := filepath.Join(dir, "device")
	if _, err := newStore(t, devDir).GenerateInto("laptop", false, &sealedfile.Provisioner{
		Path: SealedPath(devDir), PIN: NewPassphrase(short, nil),
	}); !errors.Is(err, ErrPassphraseTooShort) {
		t.Fatalf("GenerateInto with a short passphrase: %v", err)
	}
	if _, err := os.Stat(SealedPath(devDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused passphrase left a sealed file: %v", err)
	}
	if _, err := newStore(t, devDir).Get(""); !errors.Is(err, device.ErrNoDevice) {
		t.Fatalf("a refused passphrase left a device: %v", err)
	}
}

// RemoveDevice removes a custody device's sealed file before its record: an
// unlink that fails leaves both, so the device is still listed and the remove
// can be retried, rather than orphaning the sealed keys behind no record. Not
// parallel: it replaces removeFile.
func TestRemoveDeviceRemovesTheSealedFileFirst(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "device")
	store := newStore(t, dir)
	dev, err := store.GenerateInto("laptop", false, keysProvisioner{softwareKeys(t)})
	if err != nil {
		t.Fatal(err)
	}
	// RemoveDevice reads only the record, so the sealed file's bytes do not
	// matter here.
	if err := os.WriteFile(SealedPath(dir), []byte("sealed keys"), 0o600); err != nil {
		t.Fatal(err)
	}

	unlinkErr := errors.New("injected unlink failure")
	removeFile = func(path string) error {
		if path == SealedPath(dir) {
			return unlinkErr
		}
		return os.Remove(path)
	}
	t.Cleanup(func() { removeFile = os.Remove })
	if _, err := RemoveDevice(store, dev.ID); !errors.Is(err, unlinkErr) {
		t.Fatalf("RemoveDevice with a failing unlink: %v, want the unlink error", err)
	}
	if _, err := os.Stat(SealedPath(dir)); err != nil {
		t.Fatalf("the sealed file is gone after a failed remove: %v", err)
	}
	if _, err := store.Get(dev.ID); err != nil {
		t.Fatalf("the record is gone after a failed remove, so nothing can retry it: %v", err)
	}

	removeFile = os.Remove
	if _, err := RemoveDevice(store, dev.ID); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if _, err := os.Stat(SealedPath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the sealed file survived the retry: %v", err)
	}
	if _, err := store.Get(""); !errors.Is(err, device.ErrNoDevice) {
		t.Fatalf("the record survived the retry: %v", err)
	}
	if _, err := RemoveDevice(store, ""); err == nil {
		t.Fatal("RemoveDevice with no id was accepted")
	}
}
