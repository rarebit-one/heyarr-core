package pairing

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
)

// A fixed salt so the derivation is a fact, not a roll of the dice: every test
// that is not specifically about salt freshness uses this one, so a failure is
// reproducible from the source alone.
var testSalt = bytes.Repeat([]byte{0xA5}, SaltLen)

// key returns a deterministic-enough keypair's public half. The seed makes each
// call in a test distinguishable in a failure message; the value is otherwise an
// ordinary 32-byte ed25519 key.
func key(t *testing.T, seed byte) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{seed}, ed25519.SeedSize)))
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	return pub
}

// deriveOK derives and fails the test on any error — the honest-path helper. It
// takes bare signing keys and pairs with no encryption key (the v1-shaped path),
// so the many tests that are about the signing-key binding and the salt stay
// unchanged across the v2 bump; the encryption-key binding has its own tests below.
func deriveOK(t *testing.T, initiator, responder ed25519.PublicKey, salt []byte) SAS {
	t.Helper()
	s, err := Derive(Keys{Sign: initiator}, Keys{Sign: responder}, salt)
	if err != nil {
		t.Fatalf("Derive refused a well-formed input: %v", err)
	}
	return s
}

// encKey returns a deterministic 32-byte X25519-shaped public key for a seed —
// the encryption half of a device's key set. Pairing only hashes the bytes, so a
// fixed pattern is a fine stand-in for a real X25519 point here.
func encKey(seed byte) []byte {
	return bytes.Repeat([]byte{seed}, EncKeySize)
}

// The honest path: two devices that exchanged the SAME two keys under the SAME
// salt derive the SAME string. This is the "iff" from the other side — the tests
// below prove the "only if" by substituting each key.
func TestSameInputsSameStringOnBothSides(t *testing.T) {
	t.Parallel()
	initiator, responder := key(t, 1), key(t, 2)

	// The old device slots (its own key, the new device's key); the new device
	// slots (the old device's key, its own). Same values, same roles → same string.
	oldSide := deriveOK(t, initiator, responder, testSalt)
	newSide := deriveOK(t, initiator, responder, testSalt)
	if oldSide != newSide {
		t.Fatalf("two honest sides derived different strings: %q vs %q", oldSide, newSide)
	}
}

// The string is exactly Digits decimal characters, leading zeros kept, and
// deterministic in its inputs — the documented shape a human and a QR both rely on.
func TestShapeAndDeterminism(t *testing.T) {
	t.Parallel()
	initiator, responder := key(t, 1), key(t, 2)

	first := deriveOK(t, initiator, responder, testSalt)
	if len(first) != Digits {
		t.Fatalf("SAS is %d chars, want %d: %q", len(first), Digits, first)
	}
	if strings.Trim(string(first), "0123456789") != "" {
		t.Fatalf("SAS has a non-decimal character: %q", first)
	}
	// Deterministic: re-deriving the same inputs is byte-identical, many times.
	for i := 0; i < 100; i++ {
		if got := deriveOK(t, initiator, responder, testSalt); got != first {
			t.Fatalf("Derive is not deterministic: run %d gave %q, want %q", i, got, first)
		}
	}
}

// Leading zeros are kept. A SAS whose numeric value is below 10^(Digits-1) must
// still be Digits wide, or the two sides could disagree on width. Brute a salt
// until the value starts with a zero — about one in ten do — and assert the width.
func TestLeadingZerosArePreserved(t *testing.T) {
	t.Parallel()
	initiator, responder := key(t, 1), key(t, 2)

	salt := bytes.Repeat([]byte{0x00}, SaltLen)
	for i := 0; i < 1000; i++ {
		salt[0] = byte(i)
		salt[1] = byte(i >> 8)
		s := deriveOK(t, initiator, responder, salt)
		if strings.HasPrefix(string(s), "0") {
			if len(s) != Digits {
				t.Fatalf("a leading-zero SAS was not padded to %d: %q", Digits, s)
			}
			return // property shown
		}
	}
	t.Skip("no leading-zero SAS found in 1000 salts; astronomically unlikely, not a failure of the code under test")
}

// 🔴 The property, direction one: a man-in-the-middle who substitutes the
// INITIATOR's key derives a different string, so the comparison fails and pairing
// is refused. If the SAS ever stopped binding the initiator key (the sabotage:
// drop it from the hash), this test is the one that fires.
func TestSubstitutedInitiatorKeyBreaksTheString(t *testing.T) {
	t.Parallel()
	initiator, responder := key(t, 1), key(t, 2)
	honest := deriveOK(t, initiator, responder, testSalt)

	attacker := key(t, 99) // Mallory's key, put in the initiator slot
	if attacker.Equal(initiator) {
		t.Fatal("test setup: attacker key collides with the honest initiator")
	}
	substituted := deriveOK(t, attacker, responder, testSalt)
	if substituted == honest {
		t.Fatalf("substituting the initiator key left the SAS unchanged (%q): the string does not bind it", honest)
	}
}

// 🔴 The property, direction two: a man-in-the-middle who substitutes the
// RESPONDER's key derives a different string. If the SAS ever stopped binding the
// responder key (the sabotage: drop it), this test fires. Both directions are
// asserted because binding only one key is exactly the "friendly-UI relay attack".
func TestSubstitutedResponderKeyBreaksTheString(t *testing.T) {
	t.Parallel()
	initiator, responder := key(t, 1), key(t, 2)
	honest := deriveOK(t, initiator, responder, testSalt)

	attacker := key(t, 99) // Mallory's key, put in the responder slot
	if attacker.Equal(responder) {
		t.Fatal("test setup: attacker key collides with the honest responder")
	}
	substituted := deriveOK(t, initiator, attacker, testSalt)
	if substituted == honest {
		t.Fatalf("substituting the responder key left the SAS unchanged (%q): the string does not bind it", honest)
	}
}

// The end-to-end relay scenario the two directions above compose into. Alice (old
// device) and Bob (new device) each talk to Mallory, believing it is the other.
// Alice derives against Mallory-in-the-responder-slot; Bob against
// Mallory-in-the-initiator-slot. Neither matches the string the two would have
// shared with no relay, and — the point — Alice's and Bob's strings differ from
// each other, so whichever reads theirs aloud, the other sees a mismatch.
func TestManInTheMiddleCannotMakeBothSidesAgree(t *testing.T) {
	t.Parallel()
	alice, bob, mallory := key(t, 1), key(t, 2), key(t, 99)

	trueSAS := deriveOK(t, alice, bob, testSalt) // the string with no relay
	aliceSees := deriveOK(t, alice, mallory, testSalt)
	bobSees := deriveOK(t, mallory, bob, testSalt)

	if aliceSees == trueSAS {
		t.Fatal("Alice's relayed string matched the true one: the responder key is not bound")
	}
	if bobSees == trueSAS {
		t.Fatal("Bob's relayed string matched the true one: the initiator key is not bound")
	}
	// The comparison humans actually do: Alice's string vs Bob's string. The relay
	// wins only if it can make these agree; it cannot without a second preimage.
	if aliceSees == bobSees {
		t.Fatalf("the two relayed sides agreed (%q): the relay defeated the SAS", aliceSees)
	}
}

// A fresh salt gives a fresh string: the same two keys under a different salt
// derive a different SAS. This is what stops precomputation and cross-session
// reuse — a string captured from one pairing says nothing about the next.
func TestDifferentSaltDifferentString(t *testing.T) {
	t.Parallel()
	initiator, responder := key(t, 1), key(t, 2)

	a := deriveOK(t, initiator, responder, testSalt)
	otherSalt := bytes.Repeat([]byte{0x5A}, SaltLen)
	b := deriveOK(t, initiator, responder, otherSalt)
	if a == b {
		t.Fatalf("two salts gave the same SAS (%q): the salt is not bound", a)
	}
}

// The roles are positional: swapping which key is the initiator and which is the
// responder generally changes the string. This is the other face of "both keys
// are bound" — the string is not a function of the unordered pair, so a relay
// cannot pass by merely re-labelling the two keys it forwards.
func TestKeyOrderIsBound(t *testing.T) {
	t.Parallel()
	initiator, responder := key(t, 1), key(t, 2)

	ab := deriveOK(t, initiator, responder, testSalt)
	ba := deriveOK(t, responder, initiator, testSalt)
	if ab == ba {
		t.Fatalf("swapping the two roles left the SAS unchanged (%q): order is not bound", ab)
	}
}

// Malformed inputs are refused with named sentinels, never hashed as-is. A
// truncated key would still produce a string, and two truncations could collide,
// so a wrong length is refused; an absent salt defeats freshness, so it is too.
func TestMalformedInputsAreRefused(t *testing.T) {
	t.Parallel()
	good := key(t, 1)
	other := key(t, 2)

	tests := []struct {
		name            string
		initiator, resp ed25519.PublicKey
		salt            []byte
		want            error
	}{
		{"short initiator", good[:ed25519.PublicKeySize-1], other, testSalt, ErrMalformedKey},
		{"long initiator", append(append([]byte{}, good...), 0x00), other, testSalt, ErrMalformedKey},
		{"nil initiator", nil, other, testSalt, ErrMalformedKey},
		{"short responder", good, other[:1], testSalt, ErrMalformedKey},
		{"nil responder", good, nil, testSalt, ErrMalformedKey},
		{"empty salt", good, other, nil, ErrShortSalt},
		{"salt one short of the minimum", good, other, make([]byte, MinSaltLen-1), ErrShortSalt},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Derive(Keys{Sign: tc.initiator}, Keys{Sign: tc.resp}, tc.salt); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}

	// A salt of exactly the minimum is accepted — the boundary is inclusive.
	if _, err := Derive(Keys{Sign: good}, Keys{Sign: other}, make([]byte, MinSaltLen)); err != nil {
		t.Fatalf("a salt of exactly MinSaltLen should be accepted, got %v", err)
	}
}

// NewSalt returns a full-length salt that Derive accepts, and a different one each
// call — the freshness the salt exists to provide.
func TestNewSaltIsFreshAndUsable(t *testing.T) {
	t.Parallel()
	a, err := NewSalt()
	if err != nil {
		t.Fatalf("NewSalt: %v", err)
	}
	if len(a) != SaltLen {
		t.Fatalf("NewSalt returned %d bytes, want %d", len(a), SaltLen)
	}
	b, err := NewSalt()
	if err != nil {
		t.Fatalf("NewSalt: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two NewSalt calls returned the same bytes")
	}
	if _, err := Derive(Keys{Sign: key(t, 1)}, Keys{Sign: key(t, 2)}, a); err != nil {
		t.Fatalf("a NewSalt salt should derive, got %v", err)
	}
}

// Grouped renders Digits split 3-4 for a human, and the raw form stays the thing
// to compare. The spaces are cosmetic — never compared.
func TestGroupedIsDisplayOnly(t *testing.T) {
	t.Parallel()
	s := deriveOK(t, key(t, 1), key(t, 2), testSalt)
	g := s.Grouped()
	if strings.ReplaceAll(g, " ", "") != s.String() {
		t.Fatalf("grouped %q is not the digits of %q with a space", g, s)
	}
	if strings.Count(g, " ") != 1 || len(g) != Digits+1 {
		t.Fatalf("grouped form is not the documented 3-4 shape: %q", g)
	}
}

// 🔴 The v2 property, direction one: a relay that substitutes the INITIATOR's
// ENCRYPTION key — forwarding the real signing key but swapping the key space
// keys would be wrapped for — derives a different string (§41, ADR-0049). If the
// SAS ever stopped binding the initiator encryption key (the sabotage: drop it
// from the hash), this test fires.
func TestSubstitutedInitiatorEncKeyBreaksTheString(t *testing.T) {
	t.Parallel()
	initiator := Keys{Sign: key(t, 1), Enc: encKey(0x11)}
	responder := Keys{Sign: key(t, 2), Enc: encKey(0x22)}

	honest, err := Derive(initiator, responder, testSalt)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	substituted, err := Derive(Keys{Sign: initiator.Sign, Enc: encKey(0x99)}, responder, testSalt)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if substituted == honest {
		t.Fatalf("substituting the initiator encryption key left the SAS unchanged (%q): it is not bound", honest)
	}
}

// 🔴 The v2 property, direction two: a relay that substitutes the RESPONDER's
// ENCRYPTION key derives a different string. Both directions are asserted because
// binding only one device's encryption key is the same friendly-UI relay hole the
// signing-key binding already closes for signing.
func TestSubstitutedResponderEncKeyBreaksTheString(t *testing.T) {
	t.Parallel()
	initiator := Keys{Sign: key(t, 1), Enc: encKey(0x11)}
	responder := Keys{Sign: key(t, 2), Enc: encKey(0x22)}

	honest, err := Derive(initiator, responder, testSalt)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	substituted, err := Derive(initiator, Keys{Sign: responder.Sign, Enc: encKey(0x99)}, testSalt)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if substituted == honest {
		t.Fatalf("substituting the responder encryption key left the SAS unchanged (%q): it is not bound", honest)
	}
}

// A device WITH an encryption key and the same device WITHOUT one derive different
// strings: the encryption key's presence is bound, so a relay cannot strip it (or
// add one) unnoticed. This is the framed-absence property — an empty encryption
// field is part of the hash, not a no-op.
func TestEncKeyPresenceIsBound(t *testing.T) {
	t.Parallel()
	initiator := Keys{Sign: key(t, 1), Enc: encKey(0x11)}
	responder := Keys{Sign: key(t, 2), Enc: encKey(0x22)}

	withEnc, err := Derive(initiator, responder, testSalt)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	withoutEnc, err := Derive(Keys{Sign: initiator.Sign}, Keys{Sign: responder.Sign}, testSalt)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if withEnc == withoutEnc {
		t.Fatal("a device with an encryption key derived the same SAS as one without: presence is not bound")
	}
}

// The honest v2 path: two sides with the same full key sets under the same salt
// derive the same string, and it is the documented Digits-wide decimal shape.
func TestV2HonestPathAgrees(t *testing.T) {
	t.Parallel()
	initiator := Keys{Sign: key(t, 1), Enc: encKey(0x11)}
	responder := Keys{Sign: key(t, 2), Enc: encKey(0x22)}

	a, err := Derive(initiator, responder, testSalt)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	b, err := Derive(initiator, responder, testSalt)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if a != b {
		t.Fatalf("two honest v2 sides derived different strings: %q vs %q", a, b)
	}
	if len(a) != Digits {
		t.Fatalf("v2 SAS is %d chars, want %d", len(a), Digits)
	}
}

// A malformed (wrong-length, non-empty) encryption key is refused, the same rule
// the signing key has — a truncated key is not something to pair on.
func TestMalformedEncKeyIsRefused(t *testing.T) {
	t.Parallel()
	good := Keys{Sign: key(t, 2), Enc: encKey(0x22)}
	for _, n := range []int{1, EncKeySize - 1, EncKeySize + 1} {
		bad := Keys{Sign: key(t, 1), Enc: bytes.Repeat([]byte{0x11}, n)}
		if _, err := Derive(bad, good, testSalt); !errors.Is(err, ErrMalformedKey) {
			t.Fatalf("a %d-byte encryption key was not refused: %v", n, err)
		}
	}
}
