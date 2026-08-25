package backup

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Algorithm names the signature scheme, written beside the signature rather
// than assumed, so a future rotation is a second accepted value rather than a
// migration (the same discipline identity.Algorithm applies to the key file).
const Algorithm = "ed25519"

// Core is everything a backup asserts about itself that a signature covers.
//
// It is separated from the signature fields so the bytes that are signed are
// exactly these and nothing else: a signature that covered its own value would
// be impossible to compute, and one that covered a mutable "where is this
// stored" field would break the moment the file moved between peers.
//
// The shape follows catalog.Meta deliberately (§52's provenance triple, plus
// what a control-plane backup additionally needs): ControllerID/SourcePeerID,
// a monotonic Generation/Version, and the read instant. See catalog.Meta for
// why all three are load-bearing rather than diagnostic.
type Core struct {
	// SourcePeerID is the peer id whose control plane this is (ADR-0044: the
	// origin peer, the one whose identity key signs the manifest). A backup
	// restored from another deployment is indistinguishable from this one's
	// without it.
	SourcePeerID string `json:"source_peer_id"`
	// Generation is the high-water mark of the control plane's meaningful state
	// at the instant the snapshot was read — the event-log seq (invariant 7),
	// excluding the backup subsystem's own bookkeeping events, so two backups of
	// identical state report the same number and only a real transition advances
	// it. Monotonic for a given peer, because the event seq is; a restore uses it
	// to refuse going backwards (catalog.ErrStaleSnapshot's rule, in the other
	// artefact).
	Generation int64 `json:"generation"`
	// SchemaVersion is the applied goose migration version. A restore into a
	// binary expecting a different version is refused — a silent schema mismatch
	// is the one failure here that corrupts rather than fails.
	SchemaVersion int64 `json:"schema_version"`
	// TakenAt is when the database was READ (VACUUM INTO's snapshot instant),
	// not when a peer stored or applied it.
	TakenAt time.Time `json:"taken_at"`
	// Digest is the BLAKE3 digest of the snapshot database file (invariant 1's
	// hash, the only statement about these bytes that does not come from a peer).
	Digest string `json:"digest"`
	// SizeBytes is the snapshot file's length, so a truncated transfer is caught
	// before the digest is even computed.
	SizeBytes int64 `json:"size_bytes"`
	// Omissions names what the backup deliberately does NOT carry, so a restored
	// node comes up with known holes rather than surprises (ADR-0044 question 6).
	Omissions []string `json:"omissions,omitempty"`
}

// Manifest is [Core] plus the signature over it.
type Manifest struct {
	Core
	// Signature is hex-encoded Ed25519 over the canonical encoding of Core.
	// Empty when the backup was taken without a signer (a single-node
	// convenience; a backup that crosses to a peer is always signed — M7-03).
	Signature string `json:"signature,omitempty"`
	Algorithm string `json:"algorithm,omitempty"`
}

// Omission reasons, named as constants so the demo and tests assert on the
// value rather than a substring (ADR-0044's Consequences: every refusal is an
// exact match, never a contains).
const (
	// OmitProviderCredentials — provider endpoints and credentials live in the
	// operator's config file, never the database, so a whole-database backup
	// cannot carry them (ADR-0044 question 6). Recorded so a restore reports the
	// hole rather than letting it surface as a failed provider call.
	OmitProviderCredentials = "provider-credentials"
)

// ErrGenerationRegressed is a restore whose backup is older than what it would
// overwrite. It mirrors catalog.ErrStaleSnapshot: monotonicity is refused at
// apply as well as recorded, because restoring the STALEST copy is a silent
// data-loss event dressed as a successful recovery (ADR-0044 question 3).
var ErrGenerationRegressed = errors.New("backup: this backup is older than the database it would restore over")

// ErrSchemaMismatch is a backup whose schema version is not the one the caller
// expects. A restore that silently runs against the wrong schema corrupts; this
// fails instead.
var ErrSchemaMismatch = errors.New("backup: the backup's schema version is not the expected one")

// ErrSignatureInvalid is a manifest whose signature does not verify against the
// provided public key. A tampered backup — one byte of the file or the manifest
// changed — is refused here (ADR-0044 question 2).
var ErrSignatureInvalid = errors.New("backup: the manifest signature does not verify")

// ErrUnsigned is a verification asked for against a manifest that carries no
// signature. Absent is distinct from invalid: a caller that required a signature
// must not have "there was none" pass as "it verified".
var ErrUnsigned = errors.New("backup: the manifest is unsigned and a signature was required")

// signingBytes is the exact byte sequence a signature covers.
//
// json.Marshal of a struct is deterministic — fields in declaration order, no
// maps — so this is a canonical encoding without a canonicalisation library.
// TestSigningBytesAreStable pins that so a Go upgrade cannot change it silently.
func (c Core) signingBytes() ([]byte, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("backup: encoding the manifest for signing: %w", err)
	}
	return b, nil
}

// sign returns a copy of the manifest with a signature over its Core.
func (c Core) sign(key ed25519.PrivateKey) (Manifest, error) {
	msg, err := c.signingBytes()
	if err != nil {
		return Manifest{}, err
	}
	sig := ed25519.Sign(key, msg)
	return Manifest{Core: c, Signature: hex.EncodeToString(sig), Algorithm: Algorithm}, nil
}

// Verify checks the manifest's signature against key.
//
// It returns [ErrUnsigned] for a manifest with no signature and
// [ErrSignatureInvalid] for one that does not verify — two different facts a
// caller acts on differently, never collapsed into a single boolean.
func (m Manifest) Verify(key ed25519.PublicKey) error {
	if m.Signature == "" {
		return ErrUnsigned
	}
	if m.Algorithm != Algorithm {
		return fmt.Errorf("backup: unknown signature algorithm %q", m.Algorithm)
	}
	sig, err := hex.DecodeString(m.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature is not hex: %s", ErrSignatureInvalid, err.Error())
	}
	msg, err := m.signingBytes()
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, msg, sig) {
		return ErrSignatureInvalid
	}
	return nil
}

// Age reports how old the backup is at the given instant. Like catalog.Meta.Age,
// a negative age (a source clock ahead of ours) is returned as measured rather
// than clamped — an operator who sees "-3m" has learned something true.
func (m Manifest) Age(now time.Time) time.Duration { return now.Sub(m.TakenAt) }

// Validate reports whether this manifest could describe a real backup, before
// any file is read. Version/generation zero is refused explicitly, as in
// catalog.Meta: "no backup" is the absence of one, never a backup at generation
// zero.
func (m Manifest) Validate() error {
	switch {
	case m.SourcePeerID == "":
		return errors.New("backup: a backup must name the peer whose control plane it is")
	case m.Generation <= 0:
		return fmt.Errorf("backup: a backup generation must be positive, got %d", m.Generation)
	case m.SchemaVersion <= 0:
		return fmt.Errorf("backup: a backup must record a positive schema version, got %d", m.SchemaVersion)
	case m.TakenAt.IsZero():
		return errors.New("backup: a backup must record when it was taken")
	case m.Digest == "":
		return errors.New("backup: a backup must record its digest")
	case m.SizeBytes <= 0:
		return fmt.Errorf("backup: a backup must record a positive size, got %d", m.SizeBytes)
	}
	return nil
}
