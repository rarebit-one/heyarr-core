package encryption

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/recovery"
)

// mustDevice draws an X25519 device keypair or fails the test — an authorised
// wrap target.
func mustDevice(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	priv, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return priv
}

// TestSealRoundTrip: a space key sealed for a device unwraps to the SAME key with
// that device's private key. This is §41 working — a wrapped key the authorised
// device can open.
func TestSealRoundTrip(t *testing.T) {
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}
	dev := mustDevice(t)

	wrapped, err := Seal(sk, dev.PublicKey())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if len(wrapped) != wrapOverhead {
		t.Fatalf("wrapped key is %d bytes, want %d", len(wrapped), wrapOverhead)
	}

	got, err := Unwrap(wrapped, dev)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.Equal(got.key, sk.key) {
		t.Fatal("the unwrapped space key differs from the sealed one")
	}
}

// TestWrappedForTwoDevicesEachUnwraps: a space key sealed separately for two
// devices is readable by each — §41's "wrapped per authorised device" — and the
// two wrapped blobs are different bytes (fresh ephemeral + nonce each), so a peer
// storing both learns nothing by comparing them.
func TestWrappedForTwoDevicesEachUnwraps(t *testing.T) {
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}
	a, b := mustDevice(t), mustDevice(t)

	wrappedA, err := Seal(sk, a.PublicKey())
	if err != nil {
		t.Fatalf("Seal a: %v", err)
	}
	wrappedB, err := Seal(sk, b.PublicKey())
	if err != nil {
		t.Fatalf("Seal b: %v", err)
	}
	if bytes.Equal(wrappedA, wrappedB) {
		t.Fatal("two wraps of one key produced identical bytes: the wrap is not randomised")
	}

	gotA, err := Unwrap(wrappedA, a)
	if err != nil {
		t.Fatalf("Unwrap a: %v", err)
	}
	gotB, err := Unwrap(wrappedB, b)
	if err != nil {
		t.Fatalf("Unwrap b: %v", err)
	}
	if !bytes.Equal(gotA.key, sk.key) || !bytes.Equal(gotB.key, sk.key) {
		t.Fatal("a device did not recover the shared space key")
	}
}

// TestThirdDeviceCannotUnwrap is the invariant: a device that is NOT a wrap target
// cannot open the key. This is the sabotage target — a seal that ignored the
// recipient key, or a peer holding a private key, would let a non-target read.
func TestThirdDeviceCannotUnwrap(t *testing.T) {
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}
	target := mustDevice(t)
	stranger := mustDevice(t)

	wrapped, err := Seal(sk, target.PublicKey())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Unwrap(wrapped, stranger); !errors.Is(err, ErrUnwrap) {
		t.Fatalf("a non-target device unwrapped the key (err=%v) — the seal does not bind the recipient", err)
	}
}

// TestPeerHoldsNoKeyAndCannotUnwrap models the peer: it holds the wrapped bytes
// and NO X25519 private key. There is no key to call Unwrap with — asserted at
// the type level, not by absence. The stored bytes are also not the space key.
func TestPeerHoldsNoKeyAndCannotUnwrap(t *testing.T) {
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}
	dev := mustDevice(t)
	wrapped, err := Seal(sk, dev.PublicKey())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// What the peer stores is not the plaintext key.
	if bytes.Contains(wrapped, sk.key) {
		t.Fatal("the wrapped blob contains the space key in the clear")
	}

	// A peer that fabricates a key it does not have cannot open it either: a
	// random private key is not the recipient.
	notTheRecipient, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a stand-in key: %v", err)
	}
	if _, err := Unwrap(wrapped, notTheRecipient); !errors.Is(err, ErrUnwrap) {
		t.Fatal("a peer opened a wrapped key with a key it minted itself")
	}
}

// TestTamperedWrapFails: flipping any byte of a wrapped blob makes it fail to
// open — the AEAD tag and the recipient-bound AAD catch it. One opaque ErrUnwrap,
// never a reason that could become an oracle.
func TestTamperedWrapFails(t *testing.T) {
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}
	dev := mustDevice(t)
	wrapped, err := Seal(sk, dev.PublicKey())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	for i := range wrapped {
		mutated := append([]byte{}, wrapped...)
		mutated[i] ^= 0x01
		if _, err := Unwrap(mutated, dev); !errors.Is(err, ErrUnwrap) {
			t.Fatalf("a one-bit change at byte %d was accepted", i)
		}
	}
}

// TestUnwrapRejectsWrongLength: a truncated or padded blob is refused before the
// AEAD, with the length error, not adopted.
func TestUnwrapRejectsWrongLength(t *testing.T) {
	dev := mustDevice(t)
	for _, n := range []int{0, 1, wrapOverhead - 1, wrapOverhead + 1} {
		if _, err := Unwrap(make([]byte, n), dev); !errors.Is(err, ErrWrongLength) && !errors.Is(err, ErrUnwrap) {
			t.Fatalf("Unwrap(%d bytes) = %v, want a length/unwrap refusal", n, err)
		}
	}
}

// TestRecoveryKeyIsAWrapTarget: a space key sealed for the user's RECOVERY
// encryption key (derived from the recovery secret) unwraps with the key
// re-derived from that same secret — the offline path a total-device-loss recovery
// takes (ADR-0022, ADR-0049). The peers hold the wrapped copy; the paper unwraps it.
func TestRecoveryKeyIsAWrapTarget(t *testing.T) {
	secret, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	recoveryKey, err := NewPrivateKey(recovery.DeriveUserEncryptionSeed(secret))
	if err != nil {
		t.Fatalf("NewPrivateKey(recovery seed): %v", err)
	}
	sk, err := NewSpaceKey()
	if err != nil {
		t.Fatalf("NewSpaceKey: %v", err)
	}

	// A space is created and its key wrapped for the recovery target (its PUBLIC
	// half is known without the secret present).
	wrapped, err := Seal(sk, recoveryKey.PublicKey())
	if err != nil {
		t.Fatalf("Seal for recovery key: %v", err)
	}

	// Later, on a bare machine, the user re-derives the recovery key from the
	// paper and unwraps what the peers held.
	reDerived, err := NewPrivateKey(recovery.DeriveUserEncryptionSeed(secret))
	if err != nil {
		t.Fatalf("re-deriving the recovery key: %v", err)
	}
	got, err := Unwrap(wrapped, reDerived)
	if err != nil {
		t.Fatalf("recovery unwrap: %v", err)
	}
	if !bytes.Equal(got.key, sk.key) {
		t.Fatal("recovery did not recover the space key")
	}
}

// TestZeroSpaceKeyIsUnusable: a zero SpaceKey cannot be sealed — it is not a real
// key, and the guard refuses it rather than sealing 32 zero bytes.
func TestZeroSpaceKeyIsUnusable(t *testing.T) {
	if !(SpaceKey{}).IsZero() {
		t.Fatal("the zero SpaceKey does not report IsZero")
	}
	dev := mustDevice(t)
	if _, err := Seal(SpaceKey{}, dev.PublicKey()); !errors.Is(err, ErrWrongLength) {
		t.Fatalf("Seal(zero key) = %v, want ErrWrongLength", err)
	}
}
