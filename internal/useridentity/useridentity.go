// Package useridentity is the CLIENT-side store for a user's root identity
// (§40, ADR-0048, ADR-0032): the one Ed25519 keypair whose public half a peer
// pins and whose private half signs enrolment certs for the person's devices.
//
// It is the counterpart to internal/device. A device key authorises nothing; a
// user identity is the root of authority that vouches for device keys. Both are
// the person's, both live 0600 in the person's own configuration directory, and
// NEITHER is ever the server's: a private key in the server's data dir is inside
// the blast radius the key exists to stay out of (ADR-0032). This package takes
// no --config, opens no database and calls no controller — enrolling the pin at
// a peer is a separate, admin-mediated act over the API.
//
// It mirrors internal/device deliberately: same key-file format and mode, same
// "the store never hands out the seed" discipline (the private key is loaded
// only inside SignCert and never returned), same record/key split so that
// everything a person may see is readable without opening the file that must
// never be shown.
package useridentity

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

	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// Algorithm names the signature scheme, identity's constant so a user key, a
// device key and a peer key are all rendered the same way (ADR-0012, #135).
const Algorithm = identity.Algorithm

// The names of the two files a user-identity store keeps, matching the device
// store's split for the same reason: the public half and the metadata can be
// read without ever opening the file that holds the seed.
const (
	// KeyFileName holds the private key. Only SignCert ever reads it.
	KeyFileName = "user_ed25519.key"
	// RecordFileName holds the public half and the metadata.
	RecordFileName = "identity.json"
)

// KeyFileMode is the only mode the private key may have — asserted on read as
// well as set on write, because a key that became group-readable is exactly as
// exposed as one written that way.
const KeyFileMode fs.FileMode = 0o600

// DirMode is the mode of the identity directory itself.
const DirMode fs.FileMode = 0o700

// keyFilePrefix makes the file self-describing and tells it apart at a glance
// from a device or peer key — the same kind of secret with a very different
// owner, and finding one where another belongs is a finding.
const keyFilePrefix = "heyarr-user-" + Algorithm + "-seed:"

// Errors this package refuses with, distinct because each calls for a different
// action: a chmod, a restore, a decision the caller has to make on purpose.
var (
	// ErrKeyPermissions is a private key readable by more than its owner.
	ErrKeyPermissions = errors.New("useridentity: the private key is readable by more than its owner")
	// ErrMalformedKey is a key or record file that is not what it should be.
	ErrMalformedKey = errors.New("useridentity: the key file is not a heyarr user identity key")
	// ErrNoIdentity is an operation that needs an identity where none exists.
	ErrNoIdentity = errors.New("useridentity: this machine has no user identity yet")
	// ErrIdentityExists is generate finding an identity already here.
	// Overwriting one is unrecoverable — every device this identity enrolled
	// verifies against its public key — so it takes an explicit --force.
	ErrIdentityExists = errors.New("useridentity: a user identity already exists here")
)

// Clock is the injected time source (ADR-0017).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Identity is one user identity, as everything outside this package sees it.
// There is deliberately no private-key field, following device.Device and
// identity.Identity: a caller that is never handed the seed cannot leak it.
type Identity struct {
	ID        string
	Name      string
	Algorithm string
	PublicKey ed25519.PublicKey
	CreatedAt time.Time
	KeyPath   string
}

// UserID renders the public key the way a cert's issuer and a grant's issuer are
// rendered: algorithm-prefixed lowercase hex. It is the principal's stable name.
func (i Identity) UserID() string { return identity.FormatPublicKey(i.PublicKey) }

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
	// Dir is the identity directory. Required, and never the server's data dir.
	Dir   string
	Clock Clock
}

// Store is the identity directory.
type Store struct {
	dir   string
	clock Clock
}

// NewStore opens a user-identity store. It creates nothing until asked to.
func NewStore(opts StoreOptions) (*Store, error) {
	if opts.Dir == "" {
		return nil, errors.New("useridentity: an identity directory is required")
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

// Generate creates this person's user identity.
func (s *Store) Generate(name string, force bool) (Identity, error) {
	existing, err := s.load()
	switch {
	case err == nil && !force:
		return Identity{}, fmt.Errorf("%w: %s (%s) was created at %s.\n"+
			"Regenerating would produce a DIFFERENT identity: every device this one enrolled "+
			"verifies against its public key %s, and a replaced key invalidates them all with no "+
			"warning at the moment it mattered.\n"+
			"Pass --force if that is what you mean",
			ErrIdentityExists, existing.ID, existing.Name, existing.CreatedAt.UTC().Format(time.RFC3339),
			existing.PublicKeyString())
	case err != nil && !errors.Is(err, ErrNoIdentity) && !force:
		// A key that is here but unreadable — wrong mode, or corrupt. Refusing
		// is the only safe answer, exactly as device.Generate does.
		return Identity{}, err
	}

	if name == "" {
		name = defaultName()
	}
	if err := os.MkdirAll(s.dir, DirMode); err != nil {
		return Identity{}, fmt.Errorf("useridentity: creating %s: %w", s.dir, err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("useridentity: generating a keypair: %w", err)
	}
	if err := writeKeyFile(s.KeyPath(), priv.Seed()); err != nil {
		return Identity{}, err
	}

	id := Identity{
		ID:        uuid.Must(uuid.NewV7()).String(),
		Name:      name,
		Algorithm: Algorithm,
		PublicKey: pub,
		CreatedAt: s.clock.Now().UTC(),
		KeyPath:   s.KeyPath(),
	}
	if err := s.writeRecord(id); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// Get returns the user identity on this machine, or ErrNoIdentity.
func (s *Store) Get() (Identity, error) { return s.load() }

// SignCert issues a user-signed enrolment cert binding devicePub AND deviceEnc
// (the device's "x25519:<hex>" encryption key, §41) to this identity, valid for
// lifetime from the store's clock (a zero lifetime uses enrolment.CertLifetime).
// deviceEnc may be empty for a device that has no encryption key (a v1-shaped
// binding). The private key is loaded here and nowhere else, and is never
// returned: signing is the only thing this store does with the seed, exactly as
// ADR-0032 keeps the seed out of every rendered value.
func (s *Store) SignCert(devicePub ed25519.PublicKey, deviceEnc string, lifetime time.Duration) (string, error) {
	priv, err := s.signingKey()
	if err != nil {
		return "", err
	}
	return enrolment.SignCert(priv, devicePub, deviceEnc, s.clock.Now().UTC(), lifetime)
}

// signingKey loads the seed and reconstitutes the private key. It is unexported
// and its result never leaves this package (SignCert consumes it immediately),
// so no caller can hold the user's seed.
func (s *Store) signingKey() (ed25519.PrivateKey, error) {
	seed, err := readKeyFile(s.KeyPath())
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// load reads the record and verifies the key file backs it.
func (s *Store) load() (Identity, error) {
	raw, err := os.ReadFile(filepath.Clean(s.RecordPath()))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Identity{}, fmt.Errorf("%w: generate one with `heyarr identity generate`", ErrNoIdentity)
		}
		return Identity{}, fmt.Errorf("useridentity: reading %s: %w", s.RecordPath(), err)
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Identity{}, fmt.Errorf("%w: %s is not an identity record: %w", ErrMalformedKey, s.RecordPath(), err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, rec.CreatedAt)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %s has an unreadable created_at: %w",
			ErrMalformedKey, s.RecordPath(), err)
	}

	seed, err := readKeyFile(s.KeyPath())
	if err != nil {
		return Identity{}, err
	}
	pub, ok := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if !ok {
		return Identity{}, fmt.Errorf("%w: the key at %s did not yield an ed25519 public key",
			ErrMalformedKey, s.KeyPath())
	}
	if got := identity.FormatPublicKey(pub); got != rec.PublicKey {
		return Identity{}, fmt.Errorf("%w: %s records %s and the key at %s is %s — "+
			"one of the two files was replaced, and adopting either would silently change this identity",
			ErrMalformedKey, s.RecordPath(), rec.PublicKey, s.KeyPath(), got)
	}

	return Identity{
		ID:        rec.ID,
		Name:      rec.Name,
		Algorithm: rec.Algorithm,
		PublicKey: pub,
		CreatedAt: createdAt,
		KeyPath:   s.KeyPath(),
	}, nil
}

// PublicKeyString renders the public key algorithm-prefixed, lowercase hex.
func (i Identity) PublicKeyString() string { return identity.FormatPublicKey(i.PublicKey) }

func (s *Store) writeRecord(id Identity) error {
	buf, err := json.MarshalIndent(record{
		ID:        id.ID,
		Name:      id.Name,
		Algorithm: id.Algorithm,
		PublicKey: id.PublicKeyString(),
		CreatedAt: id.CreatedAt.UTC().Format(time.RFC3339Nano),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("useridentity: encoding the identity record: %w", err)
	}
	if err := os.WriteFile(s.RecordPath(), append(buf, '\n'), KeyFileMode); err != nil {
		return fmt.Errorf("useridentity: writing the identity record: %w", err)
	}
	return nil
}

// writeKeyFile writes the seed at 0600 through a temp file in the same
// directory, so a crash mid-write cannot leave a truncated key a later read
// would take for a different identity. It mirrors device.writeKeyFile.
func writeKeyFile(path string, seed []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".userkey-*.tmp")
	if err != nil {
		return fmt.Errorf("useridentity: writing the private key: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(KeyFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("useridentity: writing the private key: %w", err)
	}
	if _, err := tmp.WriteString(keyFilePrefix + hex.EncodeToString(seed) + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("useridentity: writing the private key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("useridentity: writing the private key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("useridentity: writing the private key: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("useridentity: writing the private key: %w", err)
	}
	return nil
}

// readKeyFile loads the seed, refusing a key anyone but its owner can read.
func readKeyFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: the record names an identity whose private key is missing at %s",
				ErrNoIdentity, path)
		}
		return nil, fmt.Errorf("useridentity: reading the private key: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s is %#o and must be %#o — "+
			"a key another account can read is a key another account can sign your certs with. "+
			"Fix it with `chmod 600 %s`",
			ErrKeyPermissions, path, perm, KeyFileMode, path)
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("useridentity: reading the private key: %w", err)
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

// defaultName names the identity after the machine that generated it, because
// that is what a person would have typed and it is only a label.
func defaultName() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "my-identity"
	}
	return host + "-identity"
}
