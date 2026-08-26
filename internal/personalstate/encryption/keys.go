package encryption

// This file is the encryption key *type* and its rendering — the X25519
// counterpart of internal/peer/identity's Ed25519 signing key. See doc.go for
// why the two primitives are kept apart (ADR-0049).

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Algorithm names the key-agreement scheme ADR-0049 chose, written beside the
// key exactly as identity.Algorithm is written beside a signing key, so a reader
// never has to guess which primitive a rendered key belongs to.
const Algorithm = "x25519"

// SeedSize is the length of the X25519 scalar an encryption key is built from —
// the same width recovery.DeriveUserEncryptionSeed produces and crypto/ecdh's
// NewPrivateKey expects.
const SeedSize = 32

// ErrMalformedPublicKey is a string that is not a well-formed encryption public
// key: no algorithm prefix, the wrong algorithm, not lowercase hex, or the wrong
// length. It mirrors identity.ErrMalformedPublicKey so a caller handling both key
// kinds refuses each with a parallel error.
var ErrMalformedPublicKey = errors.New("encryption: malformed x25519 public key")

// curve is the one X25519 group; a package-level value so callers do not each
// spell ecdh.X25519().
func curve() ecdh.Curve { return ecdh.X25519() }

// FormatPublicKey renders an X25519 public key the way identity.FormatPublicKey
// renders a signing key: algorithm-prefixed lowercase hex. An empty key renders
// "" rather than a bare "x25519:", so a zero key cannot masquerade as a real one.
func FormatPublicKey(pub []byte) string {
	if len(pub) == 0 {
		return ""
	}
	return Algorithm + ":" + hex.EncodeToString(pub)
}

// ParsePublicKey reverses FormatPublicKey, refusing anything that is not exactly
// an "x25519:<64 hex characters>" of the right length. It rejects rather than
// lowercases a mixed-case rendering for the same reason identity does: two
// spellings of one key would compare unequal as strings and equal as bytes, and
// the identity is the bytes.
func ParsePublicKey(s string) (*ecdh.PublicKey, error) {
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
		return nil, fmt.Errorf("%w: %q names algorithm %q, and an encryption key is %q (ADR-0049)",
			ErrMalformedPublicKey, text, algo, Algorithm)
	}
	if hexed != strings.ToLower(hexed) {
		return nil, fmt.Errorf("%w: %q is not lowercase hex", ErrMalformedPublicKey, text)
	}
	raw, err := hex.DecodeString(hexed)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not hex: %w", ErrMalformedPublicKey, text, err)
	}
	pub, err := curve().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a valid x25519 point: %w", ErrMalformedPublicKey, text, err)
	}
	return pub, nil
}

// NewPrivateKey builds an X25519 private key from a 32-byte scalar — the seed a
// device draws at enrolment or that recovery.DeriveUserEncryptionSeed derives.
// It is a thin, named wrapper over crypto/ecdh so callers do not spell the curve
// and so a wrong-length seed is refused with this package's own error.
func NewPrivateKey(seed []byte) (*ecdh.PrivateKey, error) {
	if len(seed) != SeedSize {
		return nil, fmt.Errorf("%w: an x25519 seed is %d bytes, got %d", ErrMalformedPublicKey, SeedSize, len(seed))
	}
	priv, err := curve().NewPrivateKey(seed)
	if err != nil {
		return nil, fmt.Errorf("encryption: seed is not a usable x25519 key: %w", err)
	}
	return priv, nil
}

// GenerateKey draws a fresh X25519 keypair from the system CSPRNG — the device
// encryption key a device generates on enrolment (ADR-0049), the counterpart of
// ed25519.GenerateKey for the signing key.
func GenerateKey() (*ecdh.PrivateKey, error) {
	priv, err := curve().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("encryption: generating an x25519 key: %w", err)
	}
	return priv, nil
}
