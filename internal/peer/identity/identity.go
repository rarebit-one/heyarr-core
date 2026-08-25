// Package identity establishes and guards this node's peer identity (§26,
// ADR-0012, ADR-0010).
//
// The identity is an Ed25519 keypair. The private key is a file in the data
// directory; the public key and the peer id are in the database; the peer id is
// also in the CAS root marker. Three artefacts, and the whole point of this
// package is what it does when they disagree.
//
// # Why the identity is persisted twice
//
// ADR-0010: "If they ever disagree, the process refuses to start: that
// disagreement is exactly how a deployment silently ends up with two peers
// claiming one identity, and it is unrecoverable once replication has run."
//
// The scenario is mundane. A data directory restored from a backup while the
// CAS was rebuilt from elsewhere, or a config pointed at the wrong data_dir,
// gives two machines that agree they are the same peer. Once each has served
// bytes under that identity, the controller's replicas rows are attributed to a
// peer that is two machines, and afterwards nothing in the system can tell them
// apart — not the hashes, which match on both, and not the peer id, which is
// the thing that is wrong.
//
// # Why it refuses at startup rather than at first use
//
// A node that starts and fails on its first replication has already served
// reads under a contested identity. Refusal has to happen before any listener
// is bound, which is why Ensure runs in the controller's startup path and its
// error is returned rather than logged.
package identity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Algorithm names the signature scheme ADR-0012 chose. It is written beside the
// key rather than assumed, so a future rotation is a second accepted value
// rather than a migration plus a re-enrolment of every peer.
const Algorithm = "ed25519"

// KeyFileName is the private key's name within the data directory.
const KeyFileName = "peer_ed25519.key"

// KeyFileMode is the only mode the private key may have. It is asserted on
// read as well as set on write: a key that became group-readable at some point
// after it was written is exactly as exposed as one written that way.
const KeyFileMode fs.FileMode = 0o600

// keyFilePrefix makes the file self-describing. An operator who finds it should
// be able to tell what it is without guessing from its length.
const keyFilePrefix = Algorithm + "-seed:"

// Errors this package refuses with. They are distinct because they call for
// different actions from an operator, and a test that asserts "some error"
// would pass on the wrong one.
var (
	// ErrIdentityConflict is the ADR-0010 refusal: the database and the CAS
	// root marker name different peers.
	ErrIdentityConflict = errors.New("identity: the database and the CAS root marker disagree about this peer")
	// ErrKeyMismatch is the private key on disk not belonging to the public
	// key the database records for this peer.
	ErrKeyMismatch = errors.New("identity: the private key does not match this peer's recorded public key")
	// ErrKeyMissing is a peer with a public key recorded and no private key on
	// disk. It cannot authenticate to anything, and generating a fresh key
	// would silently make it a different peer to everyone that pinned it.
	ErrKeyMissing = errors.New("identity: this peer has a recorded public key and no private key")
	// ErrKeyPermissions is a private key readable by more than its owner.
	ErrKeyPermissions = errors.New("identity: the private key is readable by more than its owner")
)

// Peers is the database half of the identity.
type Peers interface {
	// SelfIdentity reports this node's peer id and its recorded public key,
	// creating the peer row on first use. The key is nil when none is
	// recorded.
	SelfIdentity(ctx context.Context) (string, []byte, error)
	// RecordSelfPublicKey stores the public key for this node, once.
	RecordSelfPublicKey(ctx context.Context, algo string, pub []byte) error
}

// Marker is the CAS half of the identity.
//
// It is an interface rather than *cas.FS so that this package can be tested
// against a marker that disagrees without corrupting a real CAS root to get
// there — and so that the comparison below has exactly one implementation.
type Marker interface {
	// MarkerPeerID is the peer the CAS root is bound to, or "" if unbound.
	MarkerPeerID() (string, error)
	// BindPeer records the peer that owns the CAS root.
	BindPeer(peerID string) error
	// MarkerPath is where the marker lives, for error messages.
	MarkerPath() string
}

// Identity is what this node proves it is.
//
// There is deliberately no private key field. Callers that need to sign get a
// signer; callers that need to be told who this node is get this, and cannot
// leak what they were never handed.
type Identity struct {
	PeerID    string
	Algorithm string
	PublicKey ed25519.PublicKey
	// KeyPath is the private key's location, for diagnostics. The path, never
	// the bytes.
	KeyPath string
}

// PublicKeyString renders the public key the way the API and the CLI show it:
// algorithm-prefixed lowercase hex, the same shape as a blob digest (ADR-0005),
// so an operator copying one between two terminals can see at a glance which
// kind of thing they are holding.
func (i Identity) PublicKeyString() string {
	if len(i.PublicKey) == 0 {
		return ""
	}
	return FormatPublicKey(i.PublicKey)
}

// FormatPublicKey renders a public key for display and for enrolment.
func FormatPublicKey(pub []byte) string {
	if len(pub) == 0 {
		return ""
	}
	return Algorithm + ":" + hex.EncodeToString(pub)
}

// ErrMalformedPublicKey is a public key that is not one: the wrong algorithm
// prefix, not hex, or not the length an Ed25519 public key has.
//
// It is exported because enrolment refuses on it (M4-04) and the refusal has
// to be distinguishable from "this key belongs to somebody else" — an operator
// who mistyped a character and an operator who pasted the wrong site's key
// need to do different things next.
var ErrMalformedPublicKey = errors.New("identity: not an ed25519 public key")

// ParsePublicKey is FormatPublicKey's inverse, and it lives beside it so the
// two cannot drift.
//
// It accepts the rendered form, "ed25519:<64 lowercase hex>", and nothing
// else. Bare hex is deliberately refused: a value with no algorithm beside it
// is a bag of bytes whose meaning lives in whoever pasted it, and enrolment is
// the one place in this system where accepting a slightly wrong shape is
// indistinguishable from pinning the wrong identity.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return nil, fmt.Errorf("%w: the public key is empty", ErrMalformedPublicKey)
	}
	algo, hexed, ok := strings.Cut(text, ":")
	if !ok {
		return nil, fmt.Errorf("%w: %q has no algorithm prefix, expected %q",
			ErrMalformedPublicKey, text, Algorithm+":<64 hex characters>")
	}
	if algo != Algorithm {
		return nil, fmt.Errorf("%w: %q names algorithm %q, and this deployment pins %q (ADR-0012)",
			ErrMalformedPublicKey, text, algo, Algorithm)
	}
	if hexed != strings.ToLower(hexed) {
		// Rejected rather than lowercased: two renderings of one key would
		// compare unequal as strings and equal as bytes, and the unique index
		// that makes a key an identity is on the bytes.
		return nil, fmt.Errorf("%w: %q is not lowercase hex", ErrMalformedPublicKey, text)
	}
	raw, err := hex.DecodeString(hexed)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not hex: %w", ErrMalformedPublicKey, text, err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: %q decodes to %d bytes, and an ed25519 public key is %d",
			ErrMalformedPublicKey, text, len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// Options configure Ensure.
type Options struct {
	// DataDir is where the private key lives. Not the CAS root: the CAS is the
	// thing an operator rebuilds or moves between filesystems, and the key
	// must not travel with it.
	DataDir string
	Peers   Peers
	CAS     Marker
	Logger  *slog.Logger
}

// Ensure establishes this node's identity, or refuses to let it start.
//
// On a fresh node it generates a keypair, writes the private key at 0600,
// records the public key in the database and binds the CAS root marker to the
// peer id. On every later start it verifies all three agree and changes
// nothing, so the public key is byte-identical across restarts.
func Ensure(ctx context.Context, opts Options) (Identity, error) {
	if opts.DataDir == "" {
		return Identity{}, errors.New("identity: a data directory is required")
	}
	if opts.Peers == nil {
		return Identity{}, errors.New("identity: a peer store is required")
	}
	if opts.CAS == nil {
		return Identity{}, errors.New("identity: a CAS root marker is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	peerID, recorded, err := opts.Peers.SelfIdentity(ctx)
	if err != nil {
		return Identity{}, err
	}

	if err := reconcileMarker(opts.CAS, peerID); err != nil {
		return Identity{}, err
	}

	pub, err := ensureKeypair(ctx, opts, peerID, recorded, log)
	if err != nil {
		return Identity{}, err
	}

	return Identity{
		PeerID:    peerID,
		Algorithm: Algorithm,
		PublicKey: pub,
		KeyPath:   KeyPath(opts.DataDir),
	}, nil
}

// reconcileMarker is the ADR-0010 refusal.
//
// It is its own function with one comparison in it on purpose: this is the
// check the ADR promised and nothing implemented for three milestones, and a
// check spread across a startup function is a check that gets edited away by
// someone refactoring the startup function.
func reconcileMarker(marker Marker, peerID string) error {
	markerID, err := marker.MarkerPeerID()
	if err != nil {
		return err
	}
	if markerID == "" {
		// A CAS root that has never been bound — a fresh install, or a root
		// created before this check existed. Adopting it is safe: no other
		// peer has claimed it.
		return marker.BindPeer(peerID)
	}
	if markerID != peerID {
		return fmt.Errorf("%w: the database says this node is peer %s, and %s says the bytes belong to peer %s.\n"+
			"Refusing to start. Two peers claiming one identity is unrecoverable once replication has run "+
			"(ADR-0010): every replicas row written under %s would describe a peer that is two machines, "+
			"and afterwards nothing can tell them apart.\n"+
			"Correct exactly one of the two before starting again:\n"+
			"  * if this machine's CAS holds the bytes and the database was restored from elsewhere, "+
			"point database.path at the database belonging to peer %s;\n"+
			"  * if this machine's database is the right one and the CAS was rebuilt or restored from "+
			"elsewhere, point cas.root at the CAS belonging to peer %s.\n"+
			"Do not edit either value to make them match — that hides the mismatch rather than resolving it, "+
			"and one of the two sets of bytes is then attributed to the wrong peer",
			ErrIdentityConflict, peerID, marker.MarkerPath(), markerID, peerID, markerID, peerID)
	}
	return nil
}

// ensureKeypair loads the private key, or generates one, and checks it against
// what the database records.
func ensureKeypair(ctx context.Context, opts Options, peerID string, recorded []byte, log *slog.Logger) (ed25519.PublicKey, error) {
	path := KeyPath(opts.DataDir)

	seed, err := readKeyFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if len(recorded) > 0 {
			return nil, fmt.Errorf("%w: peer %s has public key %s in the database and no key at %s.\n"+
				"Refusing to start: generating a fresh keypair would make this a DIFFERENT peer to every "+
				"peer that has pinned the recorded one (ADR-0012), while still using this peer's id. "+
				"Restore the private key, or enrol this node as a new peer",
				ErrKeyMissing, peerID, FormatPublicKey(recorded), path)
		}
		pub, err := generate(ctx, opts, path, log)
		if err != nil {
			return nil, err
		}
		log.Info("generated this peer's identity",
			"peer_id", peerID, "algo", Algorithm, "public_key", FormatPublicKey(pub), "key_path", path)
		return pub, nil
	case err != nil:
		return nil, err
	}

	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("identity: the generated key did not yield an ed25519 public key")
	}

	if len(recorded) == 0 {
		// A key on disk and none in the database: the database was migrated to
		// 00019 after the key was written, or an earlier start was interrupted
		// between writing the key and committing the row. Adopting the key on
		// disk is the only choice that keeps the identity stable.
		if err := opts.Peers.RecordSelfPublicKey(ctx, Algorithm, pub); err != nil {
			return nil, err
		}
		return pub, nil
	}

	if !bytes.Equal(recorded, pub) {
		return nil, fmt.Errorf("%w: the database records %s for peer %s, and the key at %s is %s.\n"+
			"Refusing to start: this node cannot prove it is the peer its own catalog says it is, so every "+
			"peer that pinned the recorded key would reject it — and any bytes it served in the meantime "+
			"would be attributed to a peer it cannot authenticate as (ADR-0012).\n"+
			"Restore the private key belonging to %s, or enrol this node as a new peer",
			ErrKeyMismatch, FormatPublicKey(recorded), peerID, path, FormatPublicKey(pub), FormatPublicKey(recorded))
	}
	return pub, nil
}

// generate creates the keypair and persists both halves.
//
// The database write happens after the file write, and that order matters: a
// crash between them leaves a key on disk and no key in the database, which the
// next start adopts. The other order leaves a public key recorded with no
// private key anywhere, which is the unrecoverable one.
func generate(ctx context.Context, opts Options, path string, _ *slog.Logger) (ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generating a keypair: %w", err)
	}
	if err := writeKeyFile(path, priv.Seed()); err != nil {
		return nil, err
	}
	if err := opts.Peers.RecordSelfPublicKey(ctx, Algorithm, pub); err != nil {
		return nil, err
	}
	return pub, nil
}

// KeyPath is where the private key lives for a given data directory.
func KeyPath(dataDir string) string { return filepath.Join(dataDir, KeyFileName) }

// ErrKeyExists is [Install] refusing to overwrite an identity already on disk.
var ErrKeyExists = errors.New("identity: a key already exists; refusing to overwrite an identity")

// Install writes an operator-supplied identity seed into a data directory, for
// disaster recovery (`recover --from-peer`, M7-04).
//
// The seed is the 32-byte ed25519 seed — the same on-disk form [Ensure]
// generates — so a node recovered with it comes back as the SAME peer (ADR-0044
// question 4). Every other node has pinned this key, and reusing it is what lets
// the fabric reconverge with no re-enrolment anywhere.
//
// It refuses to overwrite an existing key ([ErrKeyExists]): installing over a
// live identity is exactly how two machines end up claiming one, and that
// refusal belongs at the write, not only in the command above it.
func Install(dataDir string, seed []byte) error {
	if len(seed) != ed25519.SeedSize {
		return fmt.Errorf("identity: an identity seed is %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	if dataDir == "" {
		return errors.New("identity: a data directory is required to install a key")
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("identity: preparing the data directory: %w", err)
	}
	path := KeyPath(dataDir)
	switch _, err := os.Stat(path); {
	case err == nil:
		return fmt.Errorf("%w: %s", ErrKeyExists, path)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("identity: checking for an existing key: %w", err)
	}
	return writeKeyFile(path, seed)
}

// InstallFromFile reads a key file the operator kept aside — the format [Ensure]
// writes, at 0600 — and installs it (`recover --identity-key <file>`, M7-04).
func InstallFromFile(dataDir, keyFile string) error {
	seed, err := readKeyFile(keyFile)
	if err != nil {
		return err
	}
	return Install(dataDir, seed)
}

// ReadSeed reads the 32-byte identity seed from a key file — the format [Ensure]
// writes, at 0600. It is how `recover` loads the operator-supplied key without
// installing it, so the dry run can verify a backup against this node's identity
// before anything touches the data directory.
func ReadSeed(keyFile string) ([]byte, error) { return readKeyFile(keyFile) }

// writeKeyFile writes the seed at 0600 through a temporary file in the same
// directory, so that a crash mid-write cannot leave a truncated key that the
// next start would read as a different identity.
func writeKeyFile(path string, seed []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".peerkey-*.tmp")
	if err != nil {
		return fmt.Errorf("identity: writing the private key: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	// Chmod first: os.CreateTemp makes the file 0600 already, but saying so
	// here is what keeps that true if it ever stops being.
	if err := tmp.Chmod(KeyFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: writing the private key: %w", err)
	}
	if _, err := tmp.WriteString(keyFilePrefix + hex.EncodeToString(seed) + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: writing the private key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("identity: writing the private key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("identity: writing the private key: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("identity: writing the private key: %w", err)
	}
	return nil
}

// readKeyFile loads the seed, refusing a key anyone but its owner can read.
func readKeyFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("identity: reading the private key: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s is %#o, and must be %#o — "+
			"a key another account can read is a key another account can be this peer with",
			ErrKeyPermissions, path, perm, KeyFileMode)
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is derived from the configured data directory
	if err != nil {
		return nil, fmt.Errorf("identity: reading the private key: %w", err)
	}
	text := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(text, keyFilePrefix) {
		return nil, fmt.Errorf("identity: %s is not a heyarr peer key (expected it to start with %q)",
			path, keyFilePrefix)
	}
	seed, err := hex.DecodeString(strings.TrimPrefix(text, keyFilePrefix))
	if err != nil {
		return nil, fmt.Errorf("identity: %s is not a valid key: %w", path, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("identity: %s holds %d bytes of key material, want %d",
			path, len(seed), ed25519.SeedSize)
	}
	return seed, nil
}
