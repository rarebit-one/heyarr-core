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

	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
)

// Algorithm names the signature scheme. It is identity's constant rather than
// a second spelling of "ed25519": a device key and a peer key are rendered the
// same way on purpose, so one enrolment format serves both (ADR-0012, #135).
const Algorithm = identity.Algorithm

// The names of the files a device store keeps.
const (
	// KeyFileName holds the private key. Only signing ever reads it.
	KeyFileName = "device_ed25519.key"
	// RecordFileName holds the public half and the metadata. It is separate
	// from the key so that everything a person or an agent may see can be read
	// without opening the file that must never be shown.
	RecordFileName = "device.json"
	// EncryptionKeyFileName holds the device's X25519 ENCRYPTION private key —
	// a different primitive from the signing key above, for key agreement rather
	// than signing (ADR-0049). It is what space keys are wrapped for (§41): a
	// separate keypair, generated on the device, its private half never leaving
	// it. Kept in its own file, at the same 0600, so the two secrets are never
	// conflated and either can be reasoned about alone.
	EncryptionKeyFileName = "device_x25519.key"
	// CertFileName holds the user-signed enrolment cert (§40, ADR-0048), when
	// this device has been enrolled. It is what takes the `not_enrolled` label
	// off: a device that holds a valid cert authenticates as its user. It lives
	// beside the key, client-side, because a device authenticates on either
	// peer with only its own key and this cert — the server keeps its own copy
	// but issues the device nothing. Absent until `heyarr identity enrol` runs.
	CertFileName = "enrolment.cert"
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

// encKeyFilePrefix is keyFilePrefix's counterpart for the X25519 encryption key,
// naming the different primitive so the two key files can never be mistaken for
// each other even by a person reading them.
const encKeyFilePrefix = "heyarr-device-" + encryption.Algorithm + "-seed:"

// The enrolment statuses a device can report. Enum-like values rather than
// prose because a caller must be able to compare them.
const (
	// EnrolmentNotEnrolled is a device that holds no valid enrolment cert. It
	// authenticates as nobody: presenting its key alone proves possession of a
	// key nobody has vouched for.
	EnrolmentNotEnrolled = "not_enrolled"
	// EnrolmentEnrolled is a device that holds a valid, unexpired, user-signed
	// cert binding this device key to a user identity (§40, ADR-0048). It
	// authenticates as that user — and still authorises nothing on its own.
	EnrolmentEnrolled = "enrolled"
)

// NotYetAuthorising is the caveat printed for an UN-enrolled device: it has no
// user identity behind it, so it speaks for nobody and opens nothing.
const NotYetAuthorising = "this key is not enrolled with any user identity, so it authenticates as nobody. " +
	"Enrol it with `heyarr identity enrol`, or use `heyarr token create` for a credential that authorises something today"

// EnrolledNotAuthorising is the caveat printed for an ENROLLED device. The
// label came off (ADR-0032's revisit), and the honesty did not: a cert
// AUTHENTICATES and authorises nothing (ADR-0048), so an enrolled device speaks
// for its user but opens no resource without a separate, short capability grant.
const EnrolledNotAuthorising = "this device is enrolled and authenticates as its user; it still authorises " +
	"nothing on its own — a capability grant does that (ADR-0048)"

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
	// ErrCertNotForDevice is an enrolment cert whose bound device key is not
	// this machine's — a cert for someone else's device, which would enrol a
	// key this machine cannot sign a possession proof for.
	ErrCertNotForDevice = errors.New("device: the enrolment cert binds a different device key")
	// ErrNotEnrolled is an operation that needs an enrolment cert where none is
	// held — minting an authentication credential, above all.
	ErrNotEnrolled = errors.New("device: this device holds no enrolment cert")
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

	// EncryptionKey is the device's X25519 ENCRYPTION public key — the key space
	// keys are wrapped for (§41, ADR-0049). It is the raw 32-byte public half,
	// rendered "x25519:<hex>" by EncryptionKeyString. Nil for a device generated
	// before Milestone 9 (its record carries no encryption key): such a device
	// authenticates and signs as before but is not yet a wrap target until it is
	// regenerated with `heyarr device generate --force`.
	EncryptionKey []byte
	// EncryptionKeyPath is where the X25519 private key lives. The path, never
	// the bytes, like KeyPath.
	EncryptionKeyPath string

	// cert and enrolledUser hold the VALIDATED enrolment state, empty when this
	// device holds no valid cert. They are set by load() from the cert file
	// after checking it binds this device key and is unexpired — never trusted
	// as a claim the file could assert on its own. They are unexported so the
	// only way to report "enrolled" is to have re-verified the cert on this read
	// (ADR-0032: the label reflects the truth now, not a stored flag).
	cert         string
	enrolledUser string
}

// PublicKeyString renders the public key the way #135 renders a peer's:
// algorithm-prefixed lowercase hex.
func (d Device) PublicKeyString() string { return identity.FormatPublicKey(d.PublicKey) }

// EncryptionKeyString renders the X25519 encryption public key "x25519:<hex>"
// (encryption.FormatPublicKey), or "" when this device has none — the same
// algorithm-prefixed convention as the signing key, so a reader tells the two
// primitives apart at a glance (ADR-0049).
func (d Device) EncryptionKeyString() string { return encryption.FormatPublicKey(d.EncryptionKey) }

// EnrolmentStatus reports whether this key is enrolled with a user identity. It
// is [EnrolmentEnrolled] exactly when this device holds a valid, unexpired
// user-signed cert binding it to that identity (§40, ADR-0048), and
// [EnrolmentNotEnrolled] otherwise — an expired or missing cert reads as not
// enrolled, because an authentication that has lapsed is not one.
func (d Device) EnrolmentStatus() string {
	if d.cert != "" {
		return EnrolmentEnrolled
	}
	return EnrolmentNotEnrolled
}

// Unproven reports that nothing has been proved with this key. It is false once
// the device holds a valid enrolment cert: the cert IS the proof that a user
// identity vouched for this device key. Before enrolment it is true — the label
// that ADR-0032 required "because someone will trust it" otherwise — and it
// comes off in the same change that makes it untrue, not a milestone later.
func (d Device) Unproven() bool { return d.cert == "" }

// EnrolledUser is the user identity this device is enrolled under, rendered
// "ed25519:<hex>", or empty when not enrolled. It is who the device
// authenticates as.
func (d Device) EnrolledUser() string { return d.enrolledUser }

// EnrolmentCert returns the raw user-signed cert token this device holds, and
// whether it holds one. Present only when the cert validated on load.
func (d Device) EnrolmentCert() (string, bool) { return d.cert, d.cert != "" }

// AuthorisationNote is the honesty line printed wherever a device is rendered.
// It changes with enrolment — an un-enrolled device speaks for nobody, an
// enrolled one authenticates but still authorises nothing — so that the caveat
// never claims something that has stopped being true (ADR-0032).
func (d Device) AuthorisationNote() string {
	if d.cert != "" {
		return EnrolledNotAuthorising
	}
	return NotYetAuthorising
}

// record is the on-disk metadata. The public key is stored as well as derivable
// so that a key file swapped underneath it is caught rather than adopted.
type record struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Algorithm string `json:"algorithm"`
	PublicKey string `json:"public_key"`
	// EncryptionKey is the rendered X25519 public key ("x25519:<hex>"), stored so
	// a swapped encryption-key file is caught rather than adopted, exactly as
	// PublicKey guards the signing key. `omitempty` so a pre-Milestone-9 record
	// (no encryption key) round-trips unchanged and reads back as "no key".
	EncryptionKey string `json:"encryption_key,omitempty"`
	CreatedAt     string `json:"created_at"`
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

// EncryptionKeyPath is the X25519 encryption private key's location.
func (s *Store) EncryptionKeyPath() string { return filepath.Join(s.dir, EncryptionKeyFileName) }

// RecordPath is the metadata's location.
func (s *Store) RecordPath() string { return filepath.Join(s.dir, RecordFileName) }

// CertPath is the enrolment cert's location. The file exists only once the
// device has been enrolled.
func (s *Store) CertPath() string { return filepath.Join(s.dir, CertFileName) }

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
	// The X25519 encryption keypair, drawn beside the signing key: a device
	// carries two keys, one to authenticate and one to be wrapped for (ADR-0049).
	encPriv, err := encryption.GenerateKey()
	if err != nil {
		return Device{}, fmt.Errorf("device: generating an encryption keypair: %w", err)
	}
	if err := writeKeyFile(s.KeyPath(), keyFilePrefix, priv.Seed()); err != nil {
		return Device{}, err
	}
	if err := writeKeyFile(s.EncryptionKeyPath(), encKeyFilePrefix, encPriv.Bytes()); err != nil {
		return Device{}, err
	}

	dev := Device{
		ID:                uuid.Must(uuid.NewV7()).String(),
		Name:              name,
		Algorithm:         Algorithm,
		PublicKey:         pub,
		EncryptionKey:     encPriv.PublicKey().Bytes(),
		CreatedAt:         s.clock.Now().UTC(),
		KeyPath:           s.KeyPath(),
		EncryptionKeyPath: s.EncryptionKeyPath(),
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
	if err := os.Remove(s.EncryptionKeyPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Device{}, fmt.Errorf("device: removing the encryption key: %w", err)
	}
	if err := os.Remove(s.RecordPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Device{}, fmt.Errorf("device: removing the device record: %w", err)
	}
	// The cert names this device key; a new device would render it invalid on
	// the next load anyway, but leaving it behind is a stale secret-adjacent
	// file, so it goes with the key it belonged to.
	if err := os.Remove(s.CertPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Device{}, fmt.Errorf("device: removing the enrolment cert: %w", err)
	}
	return dev, nil
}

// Enrol stores a user-signed enrolment cert for this device, taking the
// `not_enrolled` label off. The cert must bind exactly this machine's device
// key and be a well-formed, unexpired user statement, so a cert for someone
// else's device — which this machine could never sign a possession proof for —
// is refused rather than written and then found useless at authentication time.
func (s *Store) Enrol(certToken string) (Device, error) {
	dev, err := s.load()
	if err != nil {
		return Device{}, err
	}
	certToken = strings.TrimSpace(certToken)
	claimed, err := enrolment.CertUser(certToken)
	if err != nil {
		return Device{}, fmt.Errorf("%w: %w", ErrMalformedKey, err)
	}
	userKey, err := identity.ParsePublicKey(claimed)
	if err != nil {
		return Device{}, fmt.Errorf("%w: the cert names an unreadable user key: %w", ErrMalformedKey, err)
	}
	cert, err := enrolment.VerifyCert(certToken, userKey, s.clock.Now().UTC())
	if err != nil {
		return Device{}, err
	}
	if cert.Device != dev.PublicKeyString() {
		return Device{}, fmt.Errorf("%w: cert binds %s, this device is %s",
			ErrCertNotForDevice, cert.Device, dev.PublicKeyString())
	}
	if err := os.WriteFile(s.CertPath(), []byte(certToken+"\n"), KeyFileMode); err != nil {
		return Device{}, fmt.Errorf("device: writing the enrolment cert: %w", err)
	}
	dev.cert = certToken
	dev.enrolledUser = cert.User
	return dev, nil
}

// Unenrol removes the held enrolment cert, returning the device to
// `not_enrolled`. The device key is untouched: unenrolling is forgetting a
// user's vouching, not destroying the key.
func (s *Store) Unenrol() (Device, error) {
	dev, err := s.load()
	if err != nil {
		return Device{}, err
	}
	if err := os.Remove(s.CertPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Device{}, fmt.Errorf("device: removing the enrolment cert: %w", err)
	}
	dev.cert = ""
	dev.enrolledUser = ""
	return dev, nil
}

// Credential assembles the value this device presents under the `Device`
// Authorization scheme: its held enrolment cert, joined to a FRESH possession
// proof signed here with the device private key, so the two are inseparable on
// the wire (ADR-0048). A zero ttl uses enrolment.PossessionTTL.
//
// The seed is loaded and consumed inside this method and never returned — the
// same discipline the rest of the package keeps — so no caller holds the device
// key merely to authenticate. The device must be enrolled: a possession proof
// without a cert authenticates nobody.
func (s *Store) Credential(now time.Time, ttl time.Duration) (string, error) {
	dev, err := s.load()
	if err != nil {
		return "", err
	}
	if dev.cert == "" {
		return "", fmt.Errorf("%w: enrol it with `heyarr identity enrol` first", ErrNotEnrolled)
	}
	seed, err := readKeyFile(s.KeyPath(), keyFilePrefix, ed25519.SeedSize)
	if err != nil {
		return "", err
	}
	proof, err := enrolment.SignPossession(ed25519.NewKeyFromSeed(seed), dev.cert, now.UTC(), ttl)
	if err != nil {
		return "", fmt.Errorf("device: signing possession: %w", err)
	}
	return dev.cert + enrolment.CredentialSeparator + proof, nil
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

	seed, err := readKeyFile(s.KeyPath(), keyFilePrefix, ed25519.SeedSize)
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

	// The encryption key, when the record carries one. It is verified the same
	// way the signing key is — the file must back the recorded public key — so a
	// swapped encryption-key file is caught, not adopted. A record without an
	// encryption key is a pre-Milestone-9 device: it loads with EncryptionKey nil
	// rather than failing, so `device list` still works and the device keeps
	// authenticating; it simply is not yet a wrap target.
	var encPub []byte
	if rec.EncryptionKey != "" {
		encSeed, err := readKeyFile(s.EncryptionKeyPath(), encKeyFilePrefix, encryption.SeedSize)
		if err != nil {
			return Device{}, err
		}
		encKey, err := encryption.NewPrivateKey(encSeed)
		if err != nil {
			return Device{}, fmt.Errorf("%w: the encryption key at %s is unusable: %w",
				ErrMalformedKey, s.EncryptionKeyPath(), err)
		}
		encPub = encKey.PublicKey().Bytes()
		if got := encryption.FormatPublicKey(encPub); got != rec.EncryptionKey {
			return Device{}, fmt.Errorf("%w: %s records encryption key %s and the key at %s is %s — "+
				"one of the two files was replaced, and adopting either would silently change what this device can be wrapped for",
				ErrMalformedKey, s.RecordPath(), rec.EncryptionKey, s.EncryptionKeyPath(), got)
		}
	}

	dev := Device{
		ID:                rec.ID,
		Name:              rec.Name,
		Algorithm:         rec.Algorithm,
		PublicKey:         pub,
		EncryptionKey:     encPub,
		CreatedAt:         createdAt,
		KeyPath:           s.KeyPath(),
		EncryptionKeyPath: s.EncryptionKeyPath(),
	}
	// A held enrolment cert takes the `not_enrolled` label off — but only if it
	// still validates. A missing file is the ordinary un-enrolled state; a
	// present-but-invalid one (expired, tampered, or bound to a different
	// device) is treated as un-enrolled rather than surfaced as an error,
	// because the honest label for a lapsed authentication is "not enrolled",
	// not a load failure that would break `device list`.
	if user, cert, ok := s.loadCert(pub); ok {
		dev.cert = cert
		dev.enrolledUser = user
	}
	return dev, nil
}

// loadCert reads the enrolment cert, if present, and returns the user it binds
// this device to together with the raw token — but only when it VALIDATES
// against the user key it names, binds exactly this device key, and is unexpired
// on the store's clock. The cert names its own user key (like a grant's issuer),
// which VerifyCert checks the signature against, so this proves the token is a
// well-formed, unexpired, self-consistent user statement binding THIS device.
// It does not prove the peer trusts that user — that is the peer's membership
// pin, checked server-side; the client-side label reflects only "I hold a valid
// cert from user U for this device", which is exactly what enrolment means here.
func (s *Store) loadCert(devicePub ed25519.PublicKey) (user, token string, ok bool) {
	raw, err := os.ReadFile(filepath.Clean(s.CertPath()))
	if err != nil {
		return "", "", false
	}
	token = strings.TrimSpace(string(raw))
	if token == "" {
		return "", "", false
	}
	claimed, err := enrolment.CertUser(token)
	if err != nil {
		return "", "", false
	}
	userKey, err := identity.ParsePublicKey(claimed)
	if err != nil {
		return "", "", false
	}
	cert, err := enrolment.VerifyCert(token, userKey, s.clock.Now().UTC())
	if err != nil {
		return "", "", false
	}
	if cert.Device != identity.FormatPublicKey(devicePub) {
		return "", "", false
	}
	return cert.User, token, true
}

func (s *Store) writeRecord(dev Device) error {
	buf, err := json.MarshalIndent(record{
		ID:            dev.ID,
		Name:          dev.Name,
		Algorithm:     dev.Algorithm,
		PublicKey:     dev.PublicKeyString(),
		EncryptionKey: dev.EncryptionKeyString(),
		CreatedAt:     dev.CreatedAt.UTC().Format(time.RFC3339Nano),
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
// read would take for a different identity. The prefix makes the file
// self-describing and is what readKeyFile checks — the signing and encryption
// keys pass their own prefixes so neither can be read as the other.
func writeKeyFile(path, prefix string, seed []byte) error {
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
	if _, err := tmp.WriteString(prefix + hex.EncodeToString(seed) + "\n"); err != nil {
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

// readKeyFile loads the seed, refusing a key anyone but its owner can read. The
// prefix and wantLen are the key's own — the signing key passes keyFilePrefix and
// ed25519.SeedSize, the encryption key encKeyFilePrefix and encryption.SeedSize —
// so a file holding the wrong kind of key is refused rather than decoded as the
// one expected.
func readKeyFile(path, prefix string, wantLen int) ([]byte, error) {
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
	if !strings.HasPrefix(text, prefix) {
		return nil, fmt.Errorf("%w: %s does not start with %q", ErrMalformedKey, path, prefix)
	}
	seed, err := hex.DecodeString(strings.TrimPrefix(text, prefix))
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not valid hex: %w", ErrMalformedKey, path, err)
	}
	if len(seed) != wantLen {
		return nil, fmt.Errorf("%w: %s holds %d bytes of key material, want %d",
			ErrMalformedKey, path, len(seed), wantLen)
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
