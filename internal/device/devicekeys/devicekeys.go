// Package devicekeys opens this machine's device store with its keys wherever
// they are held: seed files (a software device) or a passphrase-sealed file (a
// custody device, void-which-binds-go ADR-0021).
//
// Every heyarr path that opens the device store goes through [Open], so none of
// them has to know which kind of device it has. The convention is:
//
//   - A software device is what `heyarr device generate` has always made: a
//     record and two seed files in the device directory. [Open] returns a store
//     with no custody keys, exactly as device.NewStore did.
//   - A custody device's record says "key_custody": "external", and its seeds
//     exist only inside the sealed file [SealedFileName] in the same directory
//     (`heyarr device generate --custody sealedfile`). [Open] opens that file
//     and hands it to the store as StoreOptions.Keys, so Credential, SignOp,
//     Signer and Holder all work, and no seed file is ever read for it. There
//     is no fallback: a custody record without its sealed file is an error.
//
// Opening never asks for the passphrase. It is asked for lazily, on the first
// operation that needs a private key (an unwrap or a signature), and that first
// operation unlocks the sealed file for a bounded time ([Options.UnlockTTL]),
// so one command — or one gateway — asks once rather than once per signature.
// The unlocked keys are shared by every store this process opens on the same
// sealed file, so a command that opens the store twice (an authenticated client
// and a custody backend, say) still asks once.
package devicekeys

import (
	"bytes"
	"crypto"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rarebit-one/void-which-binds-go/custody"
	"github.com/rarebit-one/void-which-binds-go/custody/sealedfile"
	"github.com/rarebit-one/void-which-binds-go/device"
	"github.com/rarebit-one/void-which-binds-go/encryption"
)

// SealedFileName is the sealed file a custody device's keys live in, inside the
// device directory. It is a fixed convention rather than configuration, so the
// record and its keys travel together and nothing can point one at the other's
// wrong copy.
const SealedFileName = "device.sealed"

// The custody kinds `heyarr device generate --custody` accepts.
const (
	// Software is the default: seed files in the device directory.
	Software = "software"
	// SealedFile is a passphrase-sealed file (custody/sealedfile).
	SealedFile = "sealedfile"
)

// DefaultUnlockTTL is how long the first private operation of a command keeps
// the sealed file unlocked. A command finishes long before it; a long-running
// process (the gateway) picks its own.
const DefaultUnlockTTL = 5 * time.Minute

// ErrNoSealedFile is a custody device whose sealed file is missing. Its keys
// exist nowhere else, so there is nothing to fall back to.
var ErrNoSealedFile = errors.New("devicekeys: this device's keys are held in a sealed file, and the sealed file is missing")

// Options configure [Open].
type Options struct {
	// Dir is the device directory; "" resolves device.DefaultDir.
	Dir string
	// PIN yields the sealed file's passphrase. Nil uses [DefaultPIN]: the
	// passphrase file named by PassphraseFileEnvVar, else a prompt on the
	// terminal. It is asked only when a private operation needs it.
	PIN custody.PINFunc
	// UnlockTTL is how long a private operation keeps the sealed file unlocked;
	// zero is DefaultUnlockTTL.
	UnlockTTL time.Duration
}

// ResolveDir is the device directory dir names, or the platform default.
func ResolveDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	return device.DefaultDir()
}

// SealedPath is the sealed file of the device in dir.
func SealedPath(dir string) string { return filepath.Join(dir, SealedFileName) }

// Open opens the device store in opts.Dir. A software device (or no device at
// all) opens exactly as device.NewStore would. A custody device opens with its
// sealed file as the store's keys; the passphrase is not asked for here.
func Open(opts Options) (*device.Store, error) {
	dir, err := ResolveDir(opts.Dir)
	if err != nil {
		return nil, err
	}
	plain, err := device.NewStore(device.StoreOptions{Dir: dir})
	if err != nil {
		return nil, err
	}
	dev, err := plain.Get("")
	if err != nil || dev.KeyCustody != device.KeyCustodyExternal {
		// No device, a software device, or one that will not load: the store
		// reports its own errors, as it always has.
		return plain, nil
	}
	keys, err := sealedKeys(SealedPath(dir), opts)
	if err != nil {
		return nil, err
	}
	return device.NewStore(device.StoreOptions{Dir: dir, Keys: keys})
}

// Holder is this device's X25519 key as a custody.Holder: the software key in
// process for a software device (a client.KeyUnwrapper, as before), or the
// sealed file's holder for a custody device. A custody device's seed file is
// never read, because it has none.
func Holder(opts Options) (custody.Holder, error) {
	ds, err := Open(opts)
	if err != nil {
		return nil, err
	}
	dev, err := ds.Get("")
	if err != nil {
		return nil, err
	}
	if dev.KeyCustody == device.KeyCustodyExternal {
		return ds.Holder()
	}
	priv, err := ds.LoadEncryptionKey()
	if err != nil {
		return nil, err
	}
	return custody.NewKeyUnwrapper(priv), nil
}

// Unlock opens the device in opts.Dir and, if it is a custody device, asks for
// the passphrase now and holds its keys unlocked for ttl. A long-running process
// calls it at start, so the prompt comes before it serves rather than in the
// middle of a request. For a software device, or no device, it does nothing.
func Unlock(opts Options, ttl time.Duration) error {
	dir, err := ResolveDir(opts.Dir)
	if err != nil {
		return err
	}
	plain, err := device.NewStore(device.StoreOptions{Dir: dir})
	if err != nil {
		return err
	}
	dev, err := plain.Get("")
	if err != nil || dev.KeyCustody != device.KeyCustodyExternal {
		return nil //nolint:nilerr // no custody device: nothing to unlock, and the caller reports a missing device itself
	}
	opts.UnlockTTL = ttl
	keys, err := sealedKeys(SealedPath(dir), opts)
	if err != nil {
		return err
	}
	return keys.Unlock(ttl)
}

// RemoveSealed deletes the sealed file in dir, for removing or replacing a
// custody device: the device library leaves the custody unit to its caller. A
// missing file is not an error.
func RemoveSealed(dir string) error {
	path := SealedPath(dir)
	if err := removeFile(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("devicekeys: removing the sealed file %s: %w", path, err)
	}
	forget(path)
	return nil
}

// removeFile is os.Remove, replaceable by a test that needs an unlink to fail.
var removeFile = os.Remove

// RemoveDevice removes the device id from store, and for a custody device its
// sealed file. The sealed file goes FIRST: if it cannot be removed, the record
// stays, so the device is still listed and a retry finds it. The other order
// would leave the sealed keys on disk with no record pointing at them and no
// command to remove them.
func RemoveDevice(store *device.Store, id string) (device.Device, error) {
	if id == "" {
		// Let the library refuse it, in its words.
		return store.Remove(id)
	}
	dev, err := store.Get(id)
	if err != nil {
		return device.Device{}, err
	}
	if dev.KeyCustody == device.KeyCustodyExternal {
		if err := RemoveSealed(store.Dir()); err != nil {
			return device.Device{}, fmt.Errorf("%w; the device record is kept, so the remove can be retried", err)
		}
	}
	return store.Remove(id)
}

// The process-wide sealed files, by path. One entry holds one sealed file's
// keys and its unlock state, so every store opened on it shares one unlock.
var (
	sessionsMu sync.Mutex
	sessions   = map[string]*session{}
)

// sealedKeys returns the shared session keys for the sealed file at path,
// re-reading the file so a replaced file (a regenerated device) is not served
// from a stale entry.
func sealedKeys(path string, opts Options) (*session, error) {
	blob, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s (restore it from a backup, or replace this device with `heyarr device generate --force`)",
			ErrNoSealedFile, path)
	}
	if err != nil {
		return nil, fmt.Errorf("devicekeys: reading %s: %w", path, err)
	}
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	if s, ok := sessions[path]; ok && bytes.Equal(s.blob, blob) {
		return s, nil
	}
	pin := opts.PIN
	if pin == nil {
		pin = DefaultPIN(path)
	}
	inner, err := sealedfile.OpenBlob(blob, pin)
	if err != nil {
		return nil, fmt.Errorf("devicekeys: %s: %w", path, err)
	}
	ttl := opts.UnlockTTL
	if ttl <= 0 {
		ttl = DefaultUnlockTTL
	}
	if old, ok := sessions[path]; ok {
		_ = old.Close()
	}
	s := &session{blob: blob, inner: inner, ttl: ttl}
	sessions[path] = s
	return s, nil
}

// forget drops the session for path, zeroing anything it holds.
func forget(path string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	if s, ok := sessions[path]; ok {
		_ = s.Close()
		delete(sessions, path)
	}
}

// session is a sealed file's custody.Keys that unlocks itself on the first
// private operation, for ttl, instead of asking for the passphrase on every
// unwrap and signature. Asking for a holder or signer, or for their public
// halves, never asks.
type session struct {
	blob  []byte
	inner *sealedfile.Keys
	ttl   time.Duration

	mu    sync.Mutex
	until time.Time
}

var _ custody.Keys = (*session)(nil)

// ensure unlocks the sealed file if it is not unlocked now.
func (s *session) ensure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Before(s.until) {
		return nil
	}
	if err := s.inner.Unlock(s.ttl); err != nil {
		return err
	}
	// Measured from before the unlock, so this never outlives the inner
	// deadline: past it the inner keys would ask again on their own.
	s.until = now.Add(s.ttl)
	return nil
}

func (s *session) Holder() (custody.Holder, error) {
	h, err := s.inner.Holder()
	if err != nil {
		return nil, err
	}
	return sessionHolder{s: s, h: h}, nil
}

func (s *session) Signer(name string) (crypto.Signer, error) {
	signer, err := s.inner.Signer(name)
	if err != nil {
		return nil, err
	}
	return sessionSigner{s: s, signer: signer}, nil
}

func (s *session) Unlock(ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if err := s.inner.Unlock(ttl); err != nil {
		return err
	}
	s.until = now.Add(ttl)
	return nil
}

func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.until = time.Time{}
	return s.inner.Close()
}

type sessionHolder struct {
	s *session
	h custody.Holder
}

func (h sessionHolder) RecipientID() string { return h.h.RecipientID() }

func (h sessionHolder) Unwrap(wrapped []byte) (encryption.SpaceKey, error) {
	if err := h.s.ensure(); err != nil {
		return encryption.SpaceKey{}, err
	}
	return h.h.Unwrap(wrapped)
}

type sessionSigner struct {
	s      *session
	signer crypto.Signer
}

func (s sessionSigner) Public() crypto.PublicKey { return s.signer.Public() }

func (s sessionSigner) Sign(rand io.Reader, message []byte, opts crypto.SignerOpts) ([]byte, error) {
	// Refuse a non-pure request before asking for the passphrase.
	if err := custody.PureEd25519(opts); err != nil {
		return nil, err
	}
	if err := s.s.ensure(); err != nil {
		return nil, err
	}
	return s.signer.Sign(rand, message, opts)
}
