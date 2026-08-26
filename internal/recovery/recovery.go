// Package recovery is the recovery-secret primitive of Milestone 8 (§79,
// ADR-0022, ADR-0021).
//
// ADR-0021 makes key loss TOTAL data loss: every replica of an encrypted vault
// becomes permanently unreadable, so recovery is not a convenience but the
// load-bearing half of the feature — vaults must not ship without it. ADR-0022's
// required mechanism is a single high-entropy RECOVERY SECRET, generated once at
// account creation, displayed once, and stored by the user offline. This package
// is that secret and the derivation from it; the CLI and client-store wiring
// that generates a secret alongside an identity, and the offline restore demo,
// are a deliberate follow-up (see the M8-05 issue).
//
// # The one decision this package makes
//
// The recovery secret DETERMINISTICALLY derives the user identity's Ed25519
// signing key. That is what makes recovery restore AUTHORITY, offline: the
// reconstructed key has the SAME public half peers already pinned at enrolment
// (ADR-0048), so a recovered user re-issues device certs that verify against the
// pinned key and NOTHING is re-pinned. The chain is ADR-0022's exactly —
// secret → root key → … — with Heyarr optional at every step, because the
// derivation is a pure function of the secret and touches no process, network or
// disk.
//
// [enrolment.GenerateUserIdentity] still mints a random identity; this package
// deliberately does not change it (another track owns the client identity store,
// and wiring the deterministic path into generation is the follow-up). What this
// provides is the derivation the wiring will call.
//
// # Two properties the tests hold the line on
//
//   - Fails LOUD. A mistyped or corrupted secret is REJECTED at [ParseSecret],
//     not silently turned into a different, wrong key. The bech32m checksum is
//     the mechanism (see bech32.go) — ADR-0022's SLIP-39-over-plain-Shamir
//     concern applied to the base secret.
//   - Domain separation. The derivation is HKDF-SHA256 under a labelled info
//     string, so the SAME recovery secret can later derive the Milestone 9
//     encryption root under a DIFFERENT label with no risk of yielding the same
//     key (§79: the secret restores the identity key AND the encryption root).
package recovery

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// SecretEntropyBytes is the size of the raw recovery secret: 256 bits.
//
// It is the root of the whole vault's confidentiality under ADR-0021 and the
// sole thing between an attacker who finds the paper and the data, so it is
// sized against offline brute force with no margin for doubt — the same 256-bit
// bar a BIP-39 24-word phrase or a 1Password Secret Key sets. It is not the seed
// itself: the seed is HKDF-derived, so this size and the 32-byte Ed25519 seed
// size coincide by choice, not by construction.
const SecretEntropyBytes = 32

// checksumSymbols is bech32m's fixed six-symbol checksum.
const checksumSymbols = 6

// encodedSymbolLen is the exact number of 5-bit symbols after the "heyarr1"
// separator: the 8→5 repacking of SecretEntropyBytes (ceil(256/5) = 52) plus the
// six checksum symbols. Pinning it lets ParseSecret reject a truncated or padded
// secret as structurally MALFORMED before the checksum is consulted, so a
// checksum miss is left meaning only one thing — a transcription slip.
const encodedSymbolLen = (SecretEntropyBytes*8+4)/5 + checksumSymbols

// hrp is the bech32m human-readable prefix every encoded secret carries. It
// makes the string self-describing — an operator who finds it can tell what it
// is — and binds the checksum, so a secret pasted under a different meaning
// fails to decode.
const hrp = "heyarr"

// UserIdentityLabel is the HKDF info string that domain-separates the user
// identity signing seed from every other key the recovery secret will ever
// derive. It is versioned: the derivation is a wire format the moment one secret
// is written on paper, and a change to how the seed is derived must be a new
// label (a new key), never a silent reinterpretation of the old one.
//
// Milestone 9's encryption root will derive from the SAME secret under its own
// distinct label (e.g. "heyarr/recovery/v1/encryption-root"), which is why this
// is a label and not a bare HKDF-Expand: RFC 5869's info parameter is exactly
// the domain-separation tag, and two distinct labels yield two independent keys.
const UserIdentityLabel = "heyarr/recovery/v1/user-identity-ed25519-seed"

// The sentinel errors [ParseSecret] refuses with. They are distinct because a
// user does different things about each: a MALFORMED secret was mis-copied at
// the structural level (a wrong character, the wrong prefix, the wrong length)
// and wants re-reading; a CORRUPT secret is structurally a secret but its
// checksum does not verify — a transcription slip the BCH code caught — and also
// wants re-reading, but is the case that proves the fails-loud guarantee, so it
// is named separately and asserted with equality.
var (
	// ErrMalformedSecret is a string that is not a well-formed encoded secret:
	// not bech32m, the wrong prefix, or not the length a secret decodes to.
	ErrMalformedSecret = errors.New("recovery: malformed recovery secret")

	// ErrCorruptSecret is a well-formed encoded secret whose checksum does not
	// verify — the loud rejection of a mistyped or corrupted secret. Deriving a
	// key from it is refused; it never silently yields a different key.
	ErrCorruptSecret = errors.New("recovery: recovery secret checksum does not verify")
)

// A Secret is a validated recovery secret held in memory: SecretEntropyBytes of
// entropy and nothing else. It is produced by [GenerateSecret] at account
// creation or reconstructed by [ParseSecret] from the string the user kept, and
// it is the sole input to [DeriveUserSeed].
//
// Like [enrolment.UserIdentity] and identity.Identity, it exposes no raw-bytes
// accessor: the entropy leaves this package only as the [Secret.String] encoding
// meant to be displayed, or as a derived seed. The struct is unexported-field so
// a zero Secret cannot masquerade as a real one — only the two constructors make
// a usable value.
type Secret struct {
	entropy []byte
}

// GenerateSecret draws a fresh recovery secret from the system CSPRNG. This is
// the "generated at account creation, displayed once" of ADR-0022; the caller
// shows [Secret.String] to the user exactly once and never persists it.
func GenerateSecret() (Secret, error) {
	entropy := make([]byte, SecretEntropyBytes)
	if _, err := rand.Read(entropy); err != nil {
		return Secret{}, fmt.Errorf("recovery: drawing a recovery secret: %w", err)
	}
	return Secret{entropy: entropy}, nil
}

// String renders the secret in its transcribable, checksummed bech32m form —
// "heyarr1…" — the value the user writes down and later types back. It is the
// ONLY externalisation of the entropy, and it is intended: this string is the
// offline artifact ADR-0022 stores.
//
// A zero Secret renders as "" rather than a valid-looking empty secret.
func (s Secret) String() string {
	if len(s.entropy) == 0 {
		return ""
	}
	data, err := bytesToBase32(s.entropy)
	if err != nil {
		// Unreachable: entropy is a fixed-size byte slice, which always packs.
		panic(fmt.Sprintf("recovery: encoding a secret: %v", err))
	}
	return bech32mEncode(hrp, data)
}

// ParseSecret reconstructs a [Secret] from its encoded form, and is the loud
// gate: a mistyped secret is REJECTED here, never carried forward into a wrong
// key. It returns [ErrCorruptSecret] when the bech32m checksum does not verify —
// the transcription-error case the checksum exists to catch — and
// [ErrMalformedSecret] when the string is not a well-formed secret at all (bad
// character, wrong "heyarr" prefix, or the wrong decoded length).
func ParseSecret(encoded string) (Secret, error) {
	// Structure first: everything that can be wrong about the string WITHOUT
	// consulting the checksum — a stray character, the wrong prefix, the wrong
	// length — is ErrMalformedSecret, decided here. Only a string that is a
	// well-formed secret of exactly the right shape reaches the checksum, so a
	// checksum failure is unambiguously a transcription slip in an otherwise
	// valid secret: ErrCorruptSecret.
	gotHRP, symbols, err := bech32mSplit(encoded)
	if err != nil {
		return Secret{}, fmt.Errorf("%w: %w", ErrMalformedSecret, err)
	}
	if gotHRP != hrp {
		return Secret{}, fmt.Errorf("%w: prefix is %q, expected %q", ErrMalformedSecret, gotHRP, hrp)
	}
	if len(symbols) != encodedSymbolLen {
		return Secret{}, fmt.Errorf("%w: %d symbols, a recovery secret is %d",
			ErrMalformedSecret, len(symbols), encodedSymbolLen)
	}
	if !verifyChecksum(gotHRP, symbols) {
		return Secret{}, fmt.Errorf("%w: a transcription error was caught by the checksum", ErrCorruptSecret)
	}
	entropy, err := base32ToBytes(symbols[:len(symbols)-checksumSymbols])
	if err != nil {
		return Secret{}, fmt.Errorf("%w: %w", ErrMalformedSecret, err)
	}
	if len(entropy) != SecretEntropyBytes {
		return Secret{}, fmt.Errorf("%w: decodes to %d bytes, a recovery secret is %d",
			ErrMalformedSecret, len(entropy), SecretEntropyBytes)
	}
	return Secret{entropy: entropy}, nil
}

// DeriveUserSeed derives the 32-byte Ed25519 SEED of the user identity from the
// recovery secret. The caller composes ed25519.NewKeyFromSeed(seed) to get the
// signing key whose PUBLIC half peers already pinned — so a recovered user
// re-issues device certs that verify unchanged and nothing is re-pinned
// (ADR-0048).
//
// It is a pure function: same secret, same seed, every time, on any machine,
// with no Heyarr process, no network and no disk (ADR-0022's offline
// requirement — enforced structurally, see the import allow-list test). The
// derivation is HKDF-SHA256 under [UserIdentityLabel]; a different label yields
// an independent key, which is how the Milestone 9 encryption root will share
// this secret without colliding with the signing seed.
func DeriveUserSeed(s Secret) []byte {
	return deriveSeed(s.entropy, UserIdentityLabel)
}

// deriveSeed is the one derivation primitive, taking the domain-separation label
// as a parameter so the fixed public [DeriveUserSeed] and the future encryption
// root are the same HKDF under different labels — and so a white-box test can
// prove two labels give two keys. Output is always ed25519.SeedSize bytes.
func deriveSeed(entropy []byte, label string) []byte {
	seed, err := hkdf.Key(sha256.New, entropy, nil, label, ed25519.SeedSize)
	if err != nil {
		// Unreachable: HKDF only errors when keyLength exceeds 255·HashLen, and
		// ed25519.SeedSize (32) is far under SHA-256's 8160-byte ceiling.
		panic(fmt.Sprintf("recovery: deriving a seed: %v", err))
	}
	return seed
}

// FormatUserID renders the public key of the identity a recovery secret derives,
// in the same algorithm-prefixed hex a peer pins — a convenience so a caller (or
// a test) can confirm a recovered key matches the pinned one without reaching
// into internal/peer/identity. It is identity.FormatPublicKey by another name,
// kept here so the round-trip the tests assert has a single obvious entry point.
func FormatUserID(pub ed25519.PublicKey) string {
	return identity.FormatPublicKey(pub)
}
