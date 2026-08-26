package encryption

// wrap.go is the space key and the seal that wraps it for a device — the core of
// Invariant 6 and §41. A space key is a 256-bit symmetric key that encrypts a
// space's changes (see content.go); it is never stored at rest. Instead it is
// SEALED, separately, for each authorised device's X25519 public key and for the
// recovery encryption key, and the peers store those sealed copies (§79) holding
// no X25519 private key of their own — so a peer stores the wrapped keys and
// cannot unwrap any of them (ADR-0049).
//
// The seal is an ephemeral-static ECDH construction (NaCl box_seal in shape),
// built on stdlib crypto/ecdh and an AEAD, adding no dependency and no bespoke
// arithmetic:
//
//	e_priv, e_pub := generate ephemeral X25519
//	shared        := ECDH(e_priv, recipient_pub)
//	wrap_key      := HKDF-SHA256(shared, salt = e_pub‖recipient_pub, info = wrapInfo)
//	sealed        := AEAD_seal(wrap_key, nonce, space_key, aad = e_pub‖recipient_pub)
//	wrapped       := e_pub ‖ nonce ‖ sealed
//
// The recipient recovers shared = ECDH(recipient_priv, e_pub) and unwraps. Binding
// both public keys into the salt AND the AAD makes a wrapped key inseparable from
// the exact (ephemeral, recipient) pair it was minted for: a copy re-pointed at a
// different recipient fails to open.

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// SpaceKeySize is the length of a space key: 256 bits, the symmetric key an AEAD
// takes. It is sized like every other root secret here — against offline brute
// force with no margin for doubt.
const SpaceKeySize = 32

// wrapInfo is the HKDF domain-separation label for the space-key seal. It is
// versioned because the wrapped-key layout is a wire format the instant one space
// key is sealed and a peer stores it: a change to the construction must be a new
// label (wrapInfo/v2), never a reinterpretation of bytes already at rest. A
// post-quantum hybrid wrap, if it ever comes, is exactly such a v2 (ADR-0049).
const wrapInfo = "heyarr/space-key-wrap/v1"

// The sealed-key framing: e_pub ‖ nonce ‖ ciphertext. The ciphertext is the
// 32-byte space key plus the AEAD's 16-byte tag.
const (
	ephemeralPubLen = 32
	wrapNonceLen    = chacha20poly1305.NonceSizeX // 24: XChaCha20's extended nonce
	wrapOverhead    = ephemeralPubLen + wrapNonceLen + SpaceKeySize + chacha20poly1305.Overhead
)

// ErrWrongLength is a space key or a wrapped blob that is not the size it must be.
var ErrWrongLength = errors.New("encryption: value is the wrong length")

// ErrUnwrap is a wrapped key that did not open: the wrong recipient key, a
// corrupt or truncated blob, or a copy re-pointed at a different recipient. It is
// deliberately one opaque error — an unwrap failure must not become an oracle
// that distinguishes "wrong key" from "tampered" from "not for you".
var ErrUnwrap = errors.New("encryption: could not unwrap the space key")

// A SpaceKey is the symmetric key of one EncryptedSpace, held only in an
// authorised client's memory. The field is unexported so a zero SpaceKey cannot
// masquerade as a real one — only NewSpaceKey and a successful Unwrap produce a
// usable value — and so it cannot be marshalled into a log, a --json document or
// an event by accident (§38: the key material never leaves the client).
type SpaceKey struct {
	key []byte
}

// NewSpaceKey draws a fresh space key from the system CSPRNG. This is the key a
// new EncryptedSpace is created with, and the fresh key a revocation rotates to
// (ADR-0049).
func NewSpaceKey() (SpaceKey, error) {
	k := make([]byte, SpaceKeySize)
	if _, err := rand.Read(k); err != nil {
		return SpaceKey{}, fmt.Errorf("encryption: drawing a space key: %w", err)
	}
	return SpaceKey{key: k}, nil
}

// IsZero reports whether this is the unusable zero SpaceKey rather than a real
// one — the guard a caller uses before trusting a value it did not just mint.
func (sk SpaceKey) IsZero() bool { return len(sk.key) != SpaceKeySize }

// Seal wraps the space key for a recipient's X25519 public key, returning the
// bytes a peer stores (§79) and cannot unwrap. Every call draws a fresh ephemeral
// key and nonce, so wrapping one space key for one recipient twice yields two
// different blobs — there is no deterministic wrap to correlate.
func Seal(sk SpaceKey, recipient *ecdh.PublicKey) ([]byte, error) {
	if sk.IsZero() {
		return nil, fmt.Errorf("%w: the space key is not a usable key", ErrWrongLength)
	}
	if recipient == nil {
		return nil, errors.New("encryption: a recipient public key is required")
	}

	ephemeral, err := curve().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("encryption: drawing an ephemeral key: %w", err)
	}
	ephPub := ephemeral.PublicKey().Bytes()
	recipientPub := recipient.Bytes()

	shared, err := ephemeral.ECDH(recipient)
	if err != nil {
		return nil, fmt.Errorf("encryption: ephemeral ECDH: %w", err)
	}
	aead, err := wrapAEAD(shared, ephPub, recipientPub)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, wrapNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("encryption: drawing a wrap nonce: %w", err)
	}
	aad := append(append([]byte{}, ephPub...), recipientPub...)
	sealed := aead.Seal(nil, nonce, sk.key, aad)

	out := make([]byte, 0, wrapOverhead)
	out = append(out, ephPub...)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// Unwrap recovers the space key from a wrapped blob using the recipient's X25519
// private key, or refuses with ErrUnwrap. It never distinguishes why it failed —
// the wrong key, a truncated blob and a tampered tag all return the one error, so
// an attacker learns nothing from the failure.
func Unwrap(wrapped []byte, recipient *ecdh.PrivateKey) (SpaceKey, error) {
	if recipient == nil {
		return SpaceKey{}, errors.New("encryption: a recipient private key is required")
	}
	if len(wrapped) != wrapOverhead {
		// A length check is not an oracle: the size is public framing, not a
		// secret. It keeps a malformed blob away from the AEAD rather than leaking
		// anything about the key.
		return SpaceKey{}, fmt.Errorf("%w: wrapped key is %d bytes, want %d", ErrWrongLength, len(wrapped), wrapOverhead)
	}
	ephPub := wrapped[:ephemeralPubLen]
	nonce := wrapped[ephemeralPubLen : ephemeralPubLen+wrapNonceLen]
	sealed := wrapped[ephemeralPubLen+wrapNonceLen:]
	recipientPub := recipient.PublicKey().Bytes()

	ephKey, err := curve().NewPublicKey(ephPub)
	if err != nil {
		return SpaceKey{}, ErrUnwrap
	}
	shared, err := recipient.ECDH(ephKey)
	if err != nil {
		return SpaceKey{}, ErrUnwrap
	}
	aead, err := wrapAEAD(shared, ephPub, recipientPub)
	if err != nil {
		return SpaceKey{}, ErrUnwrap
	}
	aad := append(append([]byte{}, ephPub...), recipientPub...)
	key, err := aead.Open(nil, nonce, sealed, aad)
	if err != nil {
		return SpaceKey{}, ErrUnwrap
	}
	if len(key) != SpaceKeySize {
		return SpaceKey{}, ErrUnwrap
	}
	return SpaceKey{key: key}, nil
}

// wrapAEAD derives the wrap key from the ECDH shared secret under wrapInfo, with
// both public keys as the HKDF salt, and returns an XChaCha20-Poly1305 AEAD. The
// salt binds the wrap key to the exact (ephemeral, recipient) pair, so a shared
// secret can never be reused to key a wrap for a different pairing.
func wrapAEAD(shared, ephPub, recipientPub []byte) (cipher.AEAD, error) {
	salt := append(append([]byte{}, ephPub...), recipientPub...)
	wrapKey, err := hkdf.Key(sha256.New, shared, salt, wrapInfo, chacha20poly1305.KeySize)
	if err != nil {
		return nil, fmt.Errorf("encryption: deriving the wrap key: %w", err)
	}
	aead, err := chacha20poly1305.NewX(wrapKey)
	if err != nil {
		return nil, fmt.Errorf("encryption: constructing the wrap cipher: %w", err)
	}
	return aead, nil
}
