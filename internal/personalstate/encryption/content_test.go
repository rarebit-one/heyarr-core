package encryption

import (
	"bytes"
	"errors"
	"testing"
)

// TestContentRoundTrip: a change encrypted under a space key decrypts to the same
// plaintext under that key.
func TestContentRoundTrip(t *testing.T) {
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}
	for _, pt := range [][]byte{
		[]byte(""),
		[]byte("a"),
		[]byte("a playlist change: add item, position 3"),
		bytes.Repeat([]byte{0x5a}, 4096),
	} {
		ct, err := EncryptChange(sk, pt)
		if err != nil {
			t.Fatalf("EncryptChange: %v", err)
		}
		got, err := DecryptChange(sk, ct)
		if err != nil {
			t.Fatalf("DecryptChange: %v", err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("round trip changed the plaintext (%d bytes)", len(pt))
		}
	}
}

// TestStoredBytesAreCiphertext is the invariant a peer's storage must satisfy: the
// bytes are NOT the plaintext. This is the sabotage target for the whole plane —
// storing plaintext, or a nil cipher, makes this fail (ADR-0049, #320).
func TestStoredBytesAreCiphertext(t *testing.T) {
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}
	plaintext := []byte("private: the playlist is named after a place, and the peer must not see it")
	ct, err := EncryptChange(sk, plaintext)
	if err != nil {
		t.Fatalf("EncryptChange: %v", err)
	}
	if bytes.Contains(ct, plaintext) {
		t.Fatal("the ciphertext contains the plaintext: the change is not encrypted")
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("the stored bytes equal the plaintext")
	}
	// Two encryptions of one plaintext differ (random nonce), so a peer cannot
	// even tell two identical changes apart.
	ct2, err := EncryptChange(sk, plaintext)
	if err != nil {
		t.Fatalf("EncryptChange: %v", err)
	}
	if bytes.Equal(ct, ct2) {
		t.Fatal("two encryptions of one change are identical: the nonce is not fresh")
	}
}

// TestWrongKeyCannotDecrypt: a change encrypted under one space key does not
// decrypt under another — the confidentiality the wrap exists to protect.
func TestWrongKeyCannotDecrypt(t *testing.T) {
	sk1, err := NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}
	sk2, err := NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}
	ct, err := EncryptChange(sk1, []byte("secret change"))
	if err != nil {
		t.Fatalf("EncryptChange: %v", err)
	}
	if _, err := DecryptChange(sk2, ct); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("a change decrypted under the wrong key (err=%v)", err)
	}
}

// TestTamperedChangeFails: any single-bit change to a ciphertext makes it fail to
// decrypt — the AEAD tag catches it, with one opaque ErrDecrypt.
func TestTamperedChangeFails(t *testing.T) {
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}
	ct, err := EncryptChange(sk, []byte("integrity matters"))
	if err != nil {
		t.Fatalf("EncryptChange: %v", err)
	}
	for i := range ct {
		mutated := append([]byte{}, ct...)
		mutated[i] ^= 0x01
		if _, err := DecryptChange(sk, mutated); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("a one-bit change at byte %d was accepted", i)
		}
	}
}

// TestDecryptRejectsRunt: a ciphertext too short to hold a nonce and a tag is
// refused, not indexed into.
func TestDecryptRejectsRunt(t *testing.T) {
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}
	for _, n := range []int{0, 1, contentNonceLen, contentNonceLen + 1} {
		if _, err := DecryptChange(sk, make([]byte, n)); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("DecryptChange(%d bytes) = %v, want ErrDecrypt", n, err)
		}
	}
}
