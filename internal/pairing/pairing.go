// Package pairing is the short-authentication-string primitive of Milestone 8
// (§40, ADR-0022, ADR-0038, #305).
//
// Enrolment is pairing, not sharing (ADR-0022): a new device generates its own
// keypair, an existing authorised device authorises it, and the channel between
// them is authenticated by a SHORT AUTHENTICATION STRING — a QR code or a short
// numeric code a human reads across from one screen and confirms on the other.
// It is the pattern Signal, Matrix and 1Password converged on independently, and
// it is the out-of-band gate that makes "enrol before trust" (ADR-0032,
// ADR-0048) real rather than a sentence. Once the string matches, the old device
// signs an enrolment cert (internal/enrolment) for the new device's key and the
// peer pins the user key — but that is the caller's job, not this package's.
//
// # The one property this file exists to hold
//
// The short string binds the channel, or it binds nothing. A pairing where the
// code is not compared — or is compared but NOT bound to the two public keys
// exchanged — is a relay attack with a friendly UI. So the SAS is a commitment
// over BOTH device public keys plus a fresh per-session salt: two sides derive
// the SAME string IFF they exchanged the SAME two keys under the SAME salt. A
// man-in-the-middle who substitutes EITHER key — its own for the initiator's, or
// its own for the responder's — is hashing a different key into the string, so
// the two sides' strings differ, the human comparison fails, and pairing is
// refused. That substitution IS the whole threat: the relay carries the keys, so
// a relay that swaps one is exactly the substituted-key case, and Derive gives it
// a string that cannot match. This is proven in both directions in the tests, and
// it is the deliverable — the refusal, as much as the success.
//
// # Why the server learns nothing
//
// Every input here is PUBLIC: two public keys and a salt that may travel in the
// clear. There is no secret in this primitive, and none needs to be — the SAS's
// strength is BINDING, not secrecy. The server (ADR-0038: the relay is untrusted)
// only ever passes these public values, and cannot forge a match: to make two
// honest parties' strings agree on substituted keys it would need a second
// preimage of SHA-256 under a salt it did not choose, per pairing. So the primitive
// takes only public inputs, and that is the model of the dumb relay — nothing it
// could learn here is secret, and nothing it could swap survives the comparison.
//
// # Scope: this is a PRIMITIVE
//
// No networking, no storage, no events. Deriving the string is all this does. The
// relay/transport flow that exchanges the keys and the salt (and must commit to
// them before they are revealed, to close the rushing attack where a party picks
// its nonce after seeing the other's), the QR/numeric rendering on a device, and
// the `heyarr pair` CLI are a DELIBERATE follow-up on top of this — #305's relay
// flow and CLI. Keeping the crypto here, alone and exhaustively tested, is what
// lets that flow be built against a property that is already nailed down.
package pairing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
)

// Version is folded into the domain label below; a change to what is hashed is
// then a different string entirely, never one computed against a different
// reading of the same inputs. It is here for the reader — the label is the wire.
const Version = 2

// domain separates this hash from every other SHA-256 in the system, so a SAS can
// never collide with, say, the possession-proof cert hash (internal/enrolment)
// even on identical bytes. It carries the version, so bumping Version reshapes the
// string.
//
// v2 (Milestone 9, ADR-0049) binds each device's X25519 ENCRYPTION key as well as
// its signing key: a device is two keys now, and a pairing that authenticated the
// signing key while a relay substituted the encryption key would let the relay
// become a wrap target (§41). Binding both closes that. The bump reshapes every
// string, which is correct — a v1 and a v2 pairing are different protocols.
const domain = "heyarr/pairing/sas/v2"

// Digits is the length of the decimal string, and it is a security parameter.
//
// 10^7 possible strings is log2(10^7) ≈ 23.25 bits. The attacker model for an
// interactive SAS is ONE ONLINE GUESS per pairing, not offline brute force: the
// salt is fresh per session so nothing precomputes, and a man-in-the-middle must
// commit to its substituted key BEFORE the one-shot human comparison happens, so
// its only chance is that its forced string collides with the honest one — 1 in
// 10^7 per attempt. The accepted bar for this shape is ~20 bits (ZRTP's SAS,
// Matrix's decimal method); 7 digits clears it by ~3 bits (an 8× margin) while
// reading no harder to a human than 6 would (grouped 3-4, see [SAS.Grouped]).
// Fewer digits (4 ≈ 13 bits) makes the one online guess too cheap; far more
// (Signal's 60-digit safety number) is a fingerprint for OFFLINE comparison of
// long-lived identities, a different job than a fresh-salt interactive gate.
const Digits = 7

// space is 10^Digits: the SAS is the hash reduced modulo this.
const space = 10_000_000

// SaltLen is the size of a freshly generated session salt, and MinSaltLen is the
// smallest [Derive] will accept. The salt's only job is freshness — it makes the
// string un-precomputable and unique per pairing — so 128 bits is already far
// past what that needs, and [NewSalt] returns 256. A short or empty salt is
// refused rather than silently accepted, because an empty salt is exactly the
// value that would let a string be precomputed or reused across pairings.
const (
	SaltLen    = 32
	MinSaltLen = 16
)

// The errors this package refuses with. They are distinct because they call for
// different actions, and a test asserting "some error" would pass on the wrong
// one: a malformed key is a truncated or wrong-length key that must never be
// paired on (pairing on 20 bytes of a 32-byte key is pairing on a prefix), and a
// short salt is a caller that skipped freshness.
var (
	// ErrMalformedKey is a public key that is not ed25519.PublicKeySize bytes.
	// It is refused rather than hashed as-is: a shorter key would still yield a
	// string, and two truncations could collide, so a length that is not exactly
	// a key's length is not a key here.
	ErrMalformedKey = errors.New("pairing: a public key is not the right length")
	// ErrShortSalt is a session salt below MinSaltLen (empty included). Freshness
	// is the salt's whole purpose, and an empty salt defeats it, so it is refused
	// at derivation rather than producing a precomputable string.
	ErrShortSalt = errors.New("pairing: the session salt is too short")
)

// A SAS is the short authentication string two devices compare to authenticate a
// pairing channel. It is a fixed-width decimal string ([Digits] long, leading
// zeros kept) so that two derivations of the same inputs are byte-identical and
// compare with ==.
//
// It is NOT a secret. It is shown on a screen and read aloud or scanned, so there
// is nothing to protect with a constant-time compare — its security is that a
// wrong pairing produces a DIFFERENT one, not that its value is hidden. Callers
// compare with == (the tests do), and render it for a human with [SAS.Grouped].
type SAS string

// String returns the raw fixed-width digits, the stable form to compare and to
// encode into a QR payload.
func (s SAS) String() string { return string(s) }

// Grouped renders the string for a human to read across two screens: [Digits]
// split 3-4 as "NNN NNNN". Grouping is display only — never compare the grouped
// form, whose spaces are cosmetic; compare [SAS.String].
func (s SAS) Grouped() string {
	d := string(s)
	if len(d) != Digits {
		return d // never happens for a Derive result; don't mangle a stray value
	}
	return d[:3] + " " + d[3:]
}

// EncKeySize is the length of a device's X25519 encryption public key — the same
// 32 bytes an ed25519 key happens to be, named separately because it is a
// different primitive (§41, ADR-0049).
const EncKeySize = 32

// Keys are one device's two public keys the pairing SAS commits to: its Ed25519
// SIGNING key (§40) and its X25519 ENCRYPTION key (§41, ADR-0049). Binding both
// is v2's whole point — a relay that swapped the encryption key while forwarding
// the real signing key would become a wrap target, and the SAS must catch that
// the same way it catches a swapped signing key.
//
// Enc may be empty: a device with no encryption key (a Milestone 8 / v1 device)
// pairs with an empty encryption field, still bound (its ABSENCE is framed, so a
// relay that ADDS an encryption key changes the string). A non-empty Enc must be
// exactly EncKeySize, or Derive refuses it as malformed — the same rule the
// signing key has.
type Keys struct {
	Sign ed25519.PublicKey
	Enc  []byte
}

// Derive computes the short authentication string binding both devices' key sets
// and a fresh session salt. Both sides run it; they get the same string IFF they
// hashed the same keys, in the same roles, under the same salt.
//
// initiator is the authorising (existing) device and responder is the new device.
// The ROLES fix the order: both sides know, out of band, which is which, so both
// slot identically and derive the same string. Substituting ANY key — either
// device's signing OR encryption key, in either role — changes the preimage and
// so the string, which is the property the tests pin in every direction.
//
// The preimage is length-FRAMED: the domain label and every input is written with
// its length ahead of it. Plain concatenation would let a relay shift a byte from
// the end of one field onto the start of the next and leave the hash unchanged;
// framing makes every boundary part of what is hashed, so no such shuffle exists —
// and it is what lets an empty encryption key be bound by its absence rather than
// silently dropped. Signing keys must be exactly ed25519.PublicKeySize, an
// encryption key empty or exactly EncKeySize, and the salt at least MinSaltLen.
func Derive(initiator, responder Keys, salt []byte) (SAS, error) {
	if err := initiator.validate("initiator"); err != nil {
		return "", err
	}
	if err := responder.validate("responder"); err != nil {
		return "", err
	}
	if len(salt) < MinSaltLen {
		return "", fmt.Errorf("%w: %d bytes, want at least %d", ErrShortSalt, len(salt), MinSaltLen)
	}

	h := sha256.New()
	// Every field is length-framed so no byte can migrate across a boundary and
	// leave the digest unchanged, and so an empty encryption key is bound by its
	// framed length rather than vanishing. The domain label is framed too, so it
	// cannot be confused with the start of a key.
	writeField(h, []byte(domain))
	writeField(h, initiator.Sign)
	writeField(h, initiator.Enc)
	writeField(h, responder.Sign)
	writeField(h, responder.Enc)
	writeField(h, salt)

	// Reduce the full 256-bit digest modulo 10^Digits. Using the whole digest via
	// big.Int makes the modulo bias mathematically nil (2^256 mod 10^7 is spread
	// across a space astronomically larger than 10^7), so no digit is favoured.
	n := new(big.Int).SetBytes(h.Sum(nil))
	n.Mod(n, big.NewInt(space))
	return SAS(fmt.Sprintf("%0*d", Digits, n)), nil
}

// validate refuses a key set that is not something to pair on: a signing key that
// is not exactly a key's length, or a non-empty encryption key that is not exactly
// EncKeySize. An empty encryption key is allowed (a v1 device) and bound by its
// framed absence.
func (k Keys) validate(role string) error {
	if len(k.Sign) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: %s signing key is %d bytes, want %d", ErrMalformedKey, role, len(k.Sign), ed25519.PublicKeySize)
	}
	if len(k.Enc) != 0 && len(k.Enc) != EncKeySize {
		return fmt.Errorf("%w: %s encryption key is %d bytes, want %d or 0", ErrMalformedKey, role, len(k.Enc), EncKeySize)
	}
	return nil
}

// NewSalt returns a fresh SaltLen-byte session salt from the system CSPRNG. A new
// salt per pairing is what makes a string un-precomputable and un-reusable across
// pairings; callers generate one per session and send it in the clear (it is not
// a secret — see the package doc).
func NewSalt() ([]byte, error) {
	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("pairing: generating a session salt: %w", err)
	}
	return salt, nil
}

// writeField hashes b preceded by its length as an 8-byte big-endian count, so
// the boundary between one field and the next is itself part of the digest.
func writeField(h interface{ Write([]byte) (int, error) }, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	// sha256's Write never returns an error; ignoring it is correct here.
	_, _ = h.Write(n[:])
	_, _ = h.Write(b)
}
