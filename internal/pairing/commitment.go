// Commitment is the commit-before-reveal half of the pairing handshake (#305,
// ADR-0022, ADR-0038). It lives beside [Derive] because it defends the very same
// property from the other side.
//
// # Why a commitment is needed at all, given the SAS already binds both keys
//
// [Derive] catches a MITM who SUBSTITUTES a key: the string changes, the humans
// see different digits, and pairing is refused. That closes substitution, but
// not RUSHING. The SAS is only [Digits] long — ~23 bits — and an attacker who is
// allowed to CHOOSE its substituted key AFTER seeing the peer's revealed key and
// the salt can search ~10^7 candidate keypairs offline for one whose SAS matches
// the honest string, and present that. Twenty-three bits is nothing to a
// second-preimage search that runs on a laptop. The interactive-SAS security
// argument (one online guess per pairing) holds ONLY if neither side can pick
// its key with knowledge of the other's — which is exactly what a commitment
// enforces.
//
// So each side COMMITS to its public key — publishes H(key) — before EITHER side
// REVEALS a key. A commitment binds: it can be opened to one key and no other
// (opening it to a second key is a second preimage of SHA-256). Once committed,
// an attacker can no longer choose its key against a known target, because the
// target — the peer's key and the salt — is revealed only after the commitment
// is fixed. Its one remaining chance is that its already-committed key happens to
// collide, 1 in 10^Digits, per pairing, online. That is the property the SAS was
// always claimed to have, and the commitment is what makes the claim true.
//
// # Scope
//
// This is a primitive, like [Derive]: it computes and checks a hash and touches
// no network, storage or clock. The ORDER — commit before reveal, on both sides —
// is the transport flow's job (internal/pairflow), and it is where removing the
// [Commitment.Open] check below re-opens the rushing attack. This file only
// guarantees that a commitment opens to exactly one key.

package pairing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
)

// commitDomain separates a commitment hash from the SAS hash (and every other
// SHA-256 in the system), so a commitment can never be confused with a derived
// string even on identical bytes. It carries its own version for the same reason
// [domain] does.
const commitDomain = "heyarr/pairing/commit/v2"

// CommitmentLen is the fixed byte length of a commitment: a full SHA-256 digest.
// It is exported so the transport can length-check a commitment off the wire
// before treating it as one.
const CommitmentLen = sha256.Size

// ErrCommitmentMismatch is a revealed key that does not open the commitment its
// peer published — the rushing attacker's tell. It is a distinct sentinel
// because it is the ONE refusal that means "someone changed their key between
// committing and revealing", which is an attack, not a malformed input.
var ErrCommitmentMismatch = errors.New("pairing: revealed key does not open its commitment")

// A Commitment is a binding, hiding commitment to a device public key: the value
// a party publishes BEFORE any key is revealed, so that it cannot later reveal a
// different key than the one it committed to. It is [CommitmentLen] bytes.
//
// It is not a secret — it is broadcast through the relay — and it need not be:
// its job is BINDING (it opens to one key), and the key it hides is revealed
// moments later anyway. Compare with [Commitment.Open], never by eye.
type Commitment []byte

// Commit produces the commitment to a device's BOTH public keys — its Ed25519
// signing key and its X25519 encryption key (§41, ADR-0049). Both the initiator
// and the responder call it on their own keys at the start of the handshake, and
// each sends the RESULT — never a key — until both commitments are in.
//
// The encryption key is committed alongside the signing key so the rushing-attack
// protection covers BOTH: without it, a relay could commit to a signing key
// honestly yet substitute the encryption key at reveal time, choosing one whose
// v2 SAS collides, and get a device enrolled with the relay's encryption key as a
// wrap target (§41). The user identity has no encryption key, so enc is empty for
// the initiator's commitment — framed as a zero-length field, still bound.
//
// The preimage is length-framed exactly as [Derive]'s is, and under a distinct
// domain, so a commitment and a SAS computed over the same keys are unrelated
// values and no field boundary can be shifted. A signing key that is not exactly
// ed25519.PublicKeySize is refused rather than committed to, for the reason
// [Derive] refuses one: a commitment to a truncated key is a commitment to a
// prefix, and two truncations could collide. The encryption key is committed
// as-is (empty or its raw bytes); [Derive] enforces its length when it binds it.
func Commit(pub ed25519.PublicKey, enc []byte) (Commitment, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: key is %d bytes, want %d", ErrMalformedKey, len(pub), ed25519.PublicKeySize)
	}
	h := sha256.New()
	writeField(h, []byte(commitDomain))
	writeField(h, pub)
	writeField(h, enc)
	return h.Sum(nil), nil
}

// Open checks that pub and enc are the keys this commitment was made to, and is
// the check the transport MUST run on a peer's revealed keys before deriving the
// SAS from them. It returns nil when the keys open the commitment,
// [ErrCommitmentMismatch] when they do not, and [ErrMalformedKey] when the
// signing key or the commitment is the wrong length.
//
// Removing this call from the handshake is the sabotage that re-opens the
// rushing attack: without it, a party may commit to one pair of keys and reveal
// another, which is precisely the freedom the commitment exists to remove — and
// with the encryption key covered here, that freedom is removed for it too.
func (c Commitment) Open(pub ed25519.PublicKey, enc []byte) error {
	if len(c) != CommitmentLen {
		return fmt.Errorf("%w: commitment is %d bytes, want %d", ErrMalformedKey, len(c), CommitmentLen)
	}
	expected, err := Commit(pub, enc)
	if err != nil {
		return err
	}
	// Not a constant-time compare: the commitment is public and so is the key,
	// so there is no secret whose timing could leak. Its security is that
	// forging a match needs a SHA-256 second preimage, not that the comparison
	// hides anything.
	if !bytes.Equal(c, expected) {
		return ErrCommitmentMismatch
	}
	return nil
}
