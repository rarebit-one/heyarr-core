package encryption

// content.go encrypts a space's changes under its space key. This is the byte
// layer of Invariant 6: a CRDT change is encrypted here, on the client, before it
// is ever handed to a peer, and a peer stores and transports only the ciphertext
// (§38, §42). The peer has no key and no code path that decrypts — the merge is
// client-side, after DecryptChange (ADR-0049).
//
// The cipher is XChaCha20-Poly1305. Its 192-bit nonce is the reason: the
// personal-state plane is leaderless and multi-master (§43), so two peers mint
// changes for one space during a partition with no shared nonce counter between
// them. A random 24-byte nonce is safe at that scale where AES-GCM's 96-bit nonce
// would force a coordination the model cannot provide. The nonce is prepended to
// the ciphertext, so a change is self-contained: nonce ‖ ciphertext(+tag).

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// contentNonceLen is XChaCha20-Poly1305's extended nonce, prepended to each
// encrypted change.
const contentNonceLen = chacha20poly1305.NonceSizeX

// ErrDecrypt is a change that did not decrypt: the wrong space key, or a corrupt
// or truncated ciphertext. Like ErrUnwrap it is one opaque error — a decrypt
// failure must not tell an attacker which of those it was.
var ErrDecrypt = errors.New("encryption: could not decrypt the change")

// EncryptChange encrypts a plaintext change payload under the space key, drawing
// a fresh random nonce. The output is nonce ‖ ciphertext, self-contained and
// opaque: it is exactly what a peer stores and replicates, and it is not the
// plaintext — the property a make-demo assertion pins by checking the stored
// bytes differ from the input (ADR-0049, #320).
func EncryptChange(sk SpaceKey, plaintext []byte) ([]byte, error) {
	if sk.IsZero() {
		return nil, fmt.Errorf("%w: the space key is not a usable key", ErrWrongLength)
	}
	aead, err := chacha20poly1305.NewX(sk.key)
	if err != nil {
		return nil, fmt.Errorf("encryption: constructing the content cipher: %w", err)
	}
	nonce := make([]byte, contentNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("encryption: drawing a content nonce: %w", err)
	}
	// Seal appends the ciphertext to the nonce, so the nonce prefix and the
	// ciphertext are one allocation and the framing is unambiguous.
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

// DecryptChange reverses EncryptChange with the space key, or refuses with
// ErrDecrypt. Only a client that holds an unwrapped space key can call it
// meaningfully; a peer, holding no key, cannot.
func DecryptChange(sk SpaceKey, ciphertext []byte) ([]byte, error) {
	if sk.IsZero() {
		return nil, fmt.Errorf("%w: the space key is not a usable key", ErrWrongLength)
	}
	if len(ciphertext) < contentNonceLen+chacha20poly1305.Overhead {
		return nil, ErrDecrypt
	}
	aead, err := chacha20poly1305.NewX(sk.key)
	if err != nil {
		return nil, ErrDecrypt
	}
	nonce := ciphertext[:contentNonceLen]
	sealed := ciphertext[contentNonceLen:]
	plaintext, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}
