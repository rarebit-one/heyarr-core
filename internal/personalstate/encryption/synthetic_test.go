package encryption

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"testing"
)

// Adversarial synthetic tests for the encryption package — the opacity core
// (§38, §41, ADR-0049). The hand-written suite proves the honest round trip and
// single-case refusals; these take the attacker's position.
//
// The threat model is a MALICIOUS PEER. A peer stores wrapped keys and encrypted
// changes it cannot read (§79) and it supplies those bytes back on sync — so it
// can hand a device ANY bytes of the right shape: a wrapped blob with a crafted
// low-order ephemeral point, random garbage where a sealed key should be, a
// content ciphertext where a wrapped key is expected. None may panic, leak a
// key, or unwrap to anything; each must fail closed with one opaque error. And
// the isolation the whole plane rests on — a blob for one recipient opens for no
// other, a change under one key decrypts under no other — must hold over many
// independent draws, not just one.

// randScalarKey draws an X25519 keypair for use as a device/recipient key.
func randScalarKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestUnwrapNeverPanicsOnArbitraryBytes: a malicious peer supplies garbage where a
// wrapped key belongs. Every length — the exact wrapOverhead, shorter, longer —
// must return an error and never panic, over many random draws.
func TestUnwrapNeverPanicsOnArbitraryBytes(t *testing.T) {
	t.Parallel()
	recipient := randScalarKey(t)
	for _, n := range []int{0, 1, 16, ephemeralPubLen, wrapOverhead - 1, wrapOverhead, wrapOverhead + 1, 4096} {
		for i := 0; i < 200; i++ {
			blob := make([]byte, n)
			_, _ = rand.Read(blob)
			sk, err := Unwrap(blob, recipient)
			if err == nil {
				t.Fatalf("Unwrap accepted %d random bytes as a wrapped key", n)
			}
			if !sk.IsZero() {
				t.Fatalf("Unwrap returned a usable key on failure (len %d)", n)
			}
		}
	}
}

// TestLowOrderEphemeralIsRefused: the ephemeral public key inside a wrapped blob
// is attacker-controlled. A low-order point (the all-zero point, order 1) would
// force the ECDH shared secret to a known constant on some curves — the classic
// small-subgroup attack. crypto/ecdh must reject it, so Unwrap fails closed
// rather than deriving a predictable wrap key. Assembled by hand because Seal
// would never emit a low-order ephemeral.
func TestLowOrderEphemeralIsRefused(t *testing.T) {
	t.Parallel()
	recipient := randScalarKey(t)
	// e_pub = 32 zero bytes (a low-order point), then an arbitrary nonce and body
	// of the right total length.
	blob := make([]byte, wrapOverhead)
	// leave the first ephemeralPubLen bytes zero; fill the rest with noise.
	_, _ = rand.Read(blob[ephemeralPubLen:])
	if _, err := Unwrap(blob, recipient); !errors.Is(err, ErrUnwrap) {
		t.Fatalf("a low-order ephemeral was not refused with ErrUnwrap: %v", err)
	}
}

// TestSealRefusesLowOrderRecipient: defence in depth on the seal side — a recipient
// key is normally a pinned cert's key, but if a low-order one is ever presented,
// Seal must error rather than emit a wrap under a predictable shared secret.
func TestSealRefusesLowOrderRecipient(t *testing.T) {
	t.Parallel()
	zero, err := ecdh.X25519().NewPublicKey(make([]byte, 32))
	if err != nil {
		t.Skipf("this curve rejects a zero point at construction: %v", err)
	}
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Seal(sk, zero); err == nil {
		t.Fatal("Seal produced a wrap for a low-order recipient point")
	}
}

// TestCrossRecipientIsolationHolds: over many independent device pairs, a key
// sealed for A opens with A and NEVER with B. This is the AAD binding (both
// public keys are authenticated data) proven across draws, not one case.
func TestCrossRecipientIsolationHolds(t *testing.T) {
	t.Parallel()
	for i := 0; i < 300; i++ {
		a, b := randScalarKey(t), randScalarKey(t)
		sk, err := NewSpaceKey()
		if err != nil {
			t.Fatal(err)
		}
		wrapped, err := Seal(sk, a.PublicKey())
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		got, err := Unwrap(wrapped, a)
		if err != nil || !bytes.Equal(got.key, sk.key) {
			t.Fatalf("the intended recipient could not open its own wrap: %v", err)
		}
		if _, err := Unwrap(wrapped, b); !errors.Is(err, ErrUnwrap) {
			t.Fatalf("draw %d: a non-recipient opened the wrap", i)
		}
	}
}

// TestCrossKeyContentIsolationHolds: over many space-key pairs, a change encrypted
// under one key never decrypts under another — the confidentiality the wrap
// protects, proven across draws.
func TestCrossKeyContentIsolationHolds(t *testing.T) {
	t.Parallel()
	for i := 0; i < 300; i++ {
		k1, err := NewSpaceKey()
		if err != nil {
			t.Fatal(err)
		}
		k2, err := NewSpaceKey()
		if err != nil {
			t.Fatal(err)
		}
		ct, err := EncryptChange(k1, []byte("private change"))
		if err != nil {
			t.Fatalf("EncryptChange: %v", err)
		}
		if _, err := DecryptChange(k2, ct); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("draw %d: a change decrypted under the wrong key", i)
		}
	}
}

// TestDecryptChangeNeverPanicsOnArbitraryBytes: arbitrary peer-supplied bytes fed
// to DecryptChange fail closed, never panic — the content counterpart of the
// wrapped-key fuzz.
func TestDecryptChangeNeverPanicsOnArbitraryBytes(t *testing.T) {
	t.Parallel()
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, contentNonceLen, contentNonceLen + 1, 64, 4096} {
		for i := 0; i < 100; i++ {
			blob := make([]byte, n)
			_, _ = rand.Read(blob)
			if _, err := DecryptChange(sk, blob); !errors.Is(err, ErrDecrypt) {
				t.Fatalf("DecryptChange accepted %d random bytes", n)
			}
		}
	}
}

// TestWrapAndContentCiphertextsAreNotInterchangeable: the two constructions are
// domain-separate — a wrapped key is not a valid change ciphertext and vice
// versa, so a peer cannot smuggle one where the other is expected and have it
// decode to something.
func TestWrapAndContentCiphertextsAreNotInterchangeable(t *testing.T) {
	t.Parallel()
	recipient := randScalarKey(t)
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := Seal(sk, recipient.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	change, err := EncryptChange(sk, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}

	// A wrapped key is not a change under its own key.
	if _, err := DecryptChange(sk, wrapped); !errors.Is(err, ErrDecrypt) {
		t.Fatal("a wrapped key decoded as a change ciphertext")
	}
	// A change ciphertext is not a wrapped key for its recipient.
	if _, err := Unwrap(change, recipient); err == nil {
		t.Fatal("a change ciphertext unwrapped as a wrapped key")
	}
}

// TestManySpaceKeysAreDistinct: NewSpaceKey draws fresh 256-bit keys — no repeat
// in a large sample, the entropy the whole scheme rests on.
func TestManySpaceKeysAreDistinct(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for i := 0; i < 5000; i++ {
		sk, err := NewSpaceKey()
		if err != nil {
			t.Fatal(err)
		}
		if len(sk.key) != SpaceKeySize {
			t.Fatalf("space key is %d bytes, want %d", len(sk.key), SpaceKeySize)
		}
		if seen[string(sk.key)] {
			t.Fatal("NewSpaceKey repeated a key")
		}
		seen[string(sk.key)] = true
	}
}

// TestNilRecipientsRefused: a nil recipient key is refused, not dereferenced.
func TestNilRecipientsRefused(t *testing.T) {
	t.Parallel()
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Seal(sk, nil); err == nil {
		t.Fatal("Seal accepted a nil recipient")
	}
	if _, err := Unwrap(make([]byte, wrapOverhead), nil); err == nil {
		t.Fatal("Unwrap accepted a nil recipient")
	}
}
