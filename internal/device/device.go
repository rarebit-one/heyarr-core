package device

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// Algorithm names the signature scheme. It is identity's constant rather than
// a second spelling of "ed25519": a device key and a peer key are rendered the
// same way on purpose, so one enrolment format serves both (ADR-0012, #135).
const Algorithm = identity.Algorithm

// The names of the two files a device store keeps.
const (
	// KeyFileName holds the private key. Nothing else ever reads it.
	KeyFileName = "device_ed25519.key"
	// RecordFileName holds the public half and the metadata. It is separate
	// from the key so that everything a person or an agent may see can be read
	// without opening the file that must never be shown.
	RecordFileName = "device.json"
)

// KeyFileMode is the only mode the private key may have. It is asserted on
// read as well as set on write: a key that became group-readable after it was
// written is exactly as exposed as one written that way.
const KeyFileMode fs.FileMode = 0o600

// DirMode is the mode of the device directory itself.
const DirMode fs.FileMode = 0o700

// keyFilePrefix makes the file self-describing, and distinguishes it from a
// peer key at a glance — the two are the same kind of secret with very
// different owners, and finding one where the other belongs is a finding.
const keyFilePrefix = "heyarr-device-" + Algorithm + "-seed:"

// EnrolmentNotEnrolled is the only enrolment status this milestone can
// produce. It is an enum-like value rather than prose because a caller must be
// able to compare it, and Milestone 8 adds values beside it rather than
// replacing a sentence.
const EnrolmentNotEnrolled = "not_enrolled"

// NotYetAuthorising is the caveat, in one sentence, wherever a device record is
// rendered. See the package doc for why it is a field and not a comment.
const NotYetAuthorising = "this key authorises nothing yet: it is not enrolled with any user identity, " +
	"and every grant against a Heyarr controller remains an ADR-0011 bearer scope until Milestone 8"

// Errors this package refuses with.
//
// They are distinct because they call for different actions: one is a chmod,
// one is a restore, one is a typo, and one is a decision the caller has to make
// on purpose. A test that asserted "some error" would pass on the wrong one.
var (
	// ErrKeyPermissions is a private key readable by more than its owner.
	ErrKeyPermissions = errors.New("device: the private key is readable by more than its owner")
	// ErrMalformedKey is a key file that is not a key.
	ErrMalformedKey = errors.New("device: the key file is not a heyarr device key")
	// ErrUnknownDevice is an operation naming a device this store does not
	// hold.
	ErrUnknownDevice = errors.New("device: no such device")
	// ErrNoDevice is an operation that needs a device where none exists yet.
	ErrNoDevice = errors.New("device: this machine has no device key yet")
	// ErrDeviceExists is generate finding a key already here. Overwriting one
	// is unrecoverable — Milestone 8 wraps space keys for a public key, and a
	// replaced key cannot unwrap what the old one could — so it takes an
	// explicit --force rather than a prompt nobody reads.
	ErrDeviceExists = errors.New("device: this machine already has a device key")
)

// Clock is the injected time source (ADR-0017).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Device is one device key, as everything outside this package sees it.
//
// There is deliberately no private key field, following identity.Identity: a
// caller that is never handed the seed cannot leak it into a log line, a --json
// document or an MCP result. KeyPath is the path, never the bytes.
type Device struct {
	ID        string
	Name      string
	Algorithm string
	PublicKey ed25519.PublicKey
	CreatedAt time.Time
	KeyPath   string
}

// PublicKeyString renders the public key the way #135 renders a peer's:
// algorithm-prefixed lowercase hex.
func (d Device) PublicKeyString() string { return identity.FormatPublicKey(d.PublicKey) }

// EnrolmentStatus reports whether this key is enrolled with a user identity.
// Today it is always [EnrolmentNotEnrolled]; Milestone 8 is what makes it
// interesting.
func (d Device) EnrolmentStatus() string { return EnrolmentNotEnrolled }

// Unproven reports that nothing has yet been proved with this key. It is a
// method rather than a stored field so that no persisted record can ever claim
// otherwise before Milestone 8 makes it true.
func (d Device) Unproven() bool { return true }

// record is the on-disk metadata. The public key is stored as well as derivable
// so that a key file swapped underneath it is caught rather than adopted.
type record struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Algorithm string `json:"algorithm"`
	PublicKey string `json:"public_key"`
	CreatedAt string `json:"created_at"`
}

// StoreOptions configure a Store.
type StoreOptions struct {
	// Dir is the device directory. Required, and never the server's data_dir —
	// see the package doc.
	Dir   string
	Clock Clock
}

// Store is the device directory.
type Store struct {
	dir   string
	clock Clock
}

// NewStore opens a device store. It creates nothing until asked to.
func NewStore(opts StoreOptions) (*Store, error) {
	if opts.Dir == "" {
		return nil, errors.New("device: a device directory is required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Store{dir: filepath.Clean(opts.Dir), clock: clock}, nil
}

// Dir is where this store keeps its files.
func (s *Store) Dir() string { return s.dir }

// KeyPath is the private key's location.
func (s *Store) KeyPath() string { return filepath.Join(s.dir, KeyFileName) }

// RecordPath is the metadata's location.
func (s *Store) RecordPath() string { return filepath.Join(s.dir, RecordFileName) }

// Generate creates this machine's device key.
//
// One device, because there is one machine. The list shape exists anyway, for
// the reason ADR-0010 kept a peers table with one row in it: Milestone 8 adds
// rows rather than a schema.
func (s *Store) Generate(name string, force bool) (Device, error) {
	existing, err := s.load()
	switch {
	case err == nil && !force:
		return Device{}, fmt.Errorf("%w: %s (%s) was created at %s.\n"+
			"Regenerating would produce a DIFFERENT device: anything wrapped for %s (§41) "+
			"would become unreadable, and no warning would be printed at the moment it mattered.\n"+
			"Pass --force if that is what you mean, or remove it first with `heyarr device remove %s`",
			ErrDeviceExists, existing.ID, existing.Name, existing.CreatedAt.UTC().Format(time.RFC3339),
			existing.PublicKeyString(), existing.ID)
	case err != nil && !errors.Is(err, ErrNoDevice) && !force:
		// A key that is here but unreadable — wrong mode, or corrupt. Refusing
		// is the only safe answer: --force past a key we could not read is the
		// caller saying "destroy it anyway", and doing that silently because
		// the file happened to be broken would be the same overwrite by
		// accident.
		return Device{}, err
	}

	if name == "" {
		name = defaultName()
	}
	if err := os.MkdirAll(s.dir, DirMode); err != nil {
		return Device{}, fmt.Errorf("device: creating %s: %w", s.dir, err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Device{}, fmt.Errorf("device: generating a keypair: %w", err)
	}
	if err := writeKeyFile(s.KeyPath(), priv.Seed()); err != nil {
		return Device{}, err
	}

	dev := Device{
		ID:        uuid.Must(uuid.NewV7()).String(),
		Name:      name,
		Algorithm: Algorithm,
		PublicKey: pub,
		CreatedAt: s.clock.Now().UTC(),
		KeyPath:   s.KeyPath(),
	}
	if err := s.writeRecord(dev); err != nil {
		return Device{}, err
	}
	return dev, nil
}

// List reports the devices this machine holds — today, zero or one.
//
// It returns an empty slice rather than nil so that a caller encoding it gets
// `[]` and not `null`; those are different values to every JSON client, and the
// difference surfaces as a nil dereference in somebody's script.
func (s *Store) List() ([]Device, error) {
	dev, err := s.load()
	if errors.Is(err, ErrNoDevice) {
		return []Device{}, nil
	}
	if err != nil {
		return nil, err
	}
	return []Device{dev}, nil
}

// Get returns one device. An empty id means "the device on this machine",
// which is what a person means when they do not say.
func (s *Store) Get(id string) (Device, error) {
	dev, err := s.load()
	if err != nil {
		return Device{}, err
	}
	if id != "" && id != dev.ID {
		return Device{}, fmt.Errorf("%w: %s — this machine holds %s (%s). "+
			"Check `heyarr device list`", ErrUnknownDevice, id, dev.ID, dev.Name)
	}
	return dev, nil
}

// Remove deletes a device key, returning what it removed.
//
// The id is required and is checked before anything is deleted: an unrecoverable
// operation that accepts "whatever is there" is one that eventually runs against
// the wrong thing.
func (s *Store) Remove(id string) (Device, error) {
	if id == "" {
		return Device{}, fmt.Errorf("%w: remove needs the device id, as printed by `heyarr device list`",
			ErrUnknownDevice)
	}
	dev, err := s.Get(id)
	if err != nil {
		return Device{}, err
	}
	if err := os.Remove(s.KeyPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Device{}, fmt.Errorf("device: removing the private key: %w", err)
	}
	if err := os.Remove(s.RecordPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Device{}, fmt.Errorf("device: removing the device record: %w", err)
	}
	return dev, nil
}

// load reads the record and verifies the key file backs it.
func (s *Store) load() (Device, error) {
	raw, err := os.ReadFile(filepath.Clean(s.RecordPath()))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Device{}, fmt.Errorf("%w: generate one with `heyarr device generate`", ErrNoDevice)
		}
		return Device{}, fmt.Errorf("device: reading %s: %w", s.RecordPath(), err)
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Device{}, fmt.Errorf("%w: %s is not a device record: %w", ErrMalformedKey, s.RecordPath(), err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, rec.CreatedAt)
	if err != nil {
		return Device{}, fmt.Errorf("%w: %s has an unreadable created_at: %w",
			ErrMalformedKey, s.RecordPath(), err)
	}

	seed, err := readKeyFile(s.KeyPath())
	if err != nil {
		return Device{}, err
	}
	pub, ok := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if !ok {
		return Device{}, fmt.Errorf("%w: the key at %s did not yield an ed25519 public key",
			ErrMalformedKey, s.KeyPath())
	}
	if got := identity.FormatPublicKey(pub); got != rec.PublicKey {
		return Device{}, fmt.Errorf("%w: %s records %s and the key at %s is %s — "+
			"one of the two files was replaced, and adopting either would silently change this device's identity",
			ErrMalformedKey, s.RecordPath(), rec.PublicKey, s.KeyPath(), got)
	}

	return Device{
		ID:        rec.ID,
		Name:      rec.Name,
		Algorithm: rec.Algorithm,
		PublicKey: pub,
		CreatedAt: createdAt,
		KeyPath:   s.KeyPath(),
	}, nil
}

func (s *Store) writeRecord(dev Device) error {
	buf, err := json.MarshalIndent(record{
		ID:        dev.ID,
		Name:      dev.Name,
		Algorithm: dev.Algorithm,
		PublicKey: dev.PublicKeyString(),
		CreatedAt: dev.CreatedAt.UTC().Format(time.RFC3339Nano),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("device: encoding the device record: %w", err)
	}
	if err := os.WriteFile(s.RecordPath(), append(buf, '\n'), KeyFileMode); err != nil {
		return fmt.Errorf("device: writing the device record: %w", err)
	}
	return nil
}

// writeKeyFile writes the seed at 0600 through a temporary file in the same
// directory, so a crash mid-write cannot leave a truncated key that a later
// read would take for a different identity.
func writeKeyFile(path string, seed []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".devicekey-*.tmp")
	if err != nil {
		return fmt.Errorf("device: writing the private key: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	// Chmod first: os.CreateTemp already makes the file 0600, and saying so
	// here is what keeps that true if it ever stops being.
	if err := tmp.Chmod(KeyFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("device: writing the private key: %w", err)
	}
	if _, err := tmp.WriteString(keyFilePrefix + hex.EncodeToString(seed) + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("device: writing the private key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("device: writing the private key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("device: writing the private key: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("device: writing the private key: %w", err)
	}
	return nil
}

// readKeyFile loads the seed, refusing a key anyone but its owner can read.
func readKeyFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: the record at %s names a device whose private key is missing",
				ErrNoDevice, filepath.Dir(path))
		}
		return nil, fmt.Errorf("device: reading the private key: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s is %#o and must be %#o — "+
			"a key another account can read is a key another account can be this device with. "+
			"Fix it with `chmod 600 %s`",
			ErrKeyPermissions, path, perm, KeyFileMode, path)
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("device: reading the private key: %w", err)
	}
	text := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(text, keyFilePrefix) {
		return nil, fmt.Errorf("%w: %s does not start with %q", ErrMalformedKey, path, keyFilePrefix)
	}
	seed, err := hex.DecodeString(strings.TrimPrefix(text, keyFilePrefix))
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not valid hex: %w", ErrMalformedKey, path, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: %s holds %d bytes of key material, want %d",
			ErrMalformedKey, path, len(seed), ed25519.SeedSize)
	}
	return seed, nil
}

// defaultName names the device after the machine, because that is what a person
// would have typed.
func defaultName() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "this-device"
	}
	return host
}
