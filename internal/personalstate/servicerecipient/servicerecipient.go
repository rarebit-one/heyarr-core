// Package servicerecipient is the shared, key-free vocabulary of a service
// recipient (ADR-0104): an executor's X25519 public key registered as a
// space-key wrap target. It parses the "x25519:<hex>" form and renders the short
// fingerprint an operator compares between the executor's host and the device
// that registers the key.
//
// Both sides use it. The executor's host prints the fingerprint of the key it
// generated. The registering device prints the fingerprint of the key it was
// handed, and the controller records it. The package is stdlib only and holds
// no key: it never imports the wrap or unwrap path, so the server can use it
// without touching Invariant 6.
package servicerecipient

import (
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Prefix is the algorithm prefix of an encryption key, as void-which-binds-go's
// encryption.FormatPublicKey renders it.
const Prefix = "x25519:"

// FingerprintLabel domain-separates the fingerprint hash, so it never equals a
// hash of the same bytes taken for another purpose (a user fingerprint, a
// blob id). Changing it changes every printed fingerprint.
const FingerprintLabel = "heyarr/service-recipient-fingerprint/v1"

// fingerprintBytes is 80 bits, 16 base32 characters: short enough to read off
// one screen and type on another, long enough that a substituted key cannot be
// ground to match.
const fingerprintBytes = 10

// ErrMalformed is a recipient that is not "x25519:<64 lowercase hex characters>"
// naming a usable X25519 point.
var ErrMalformed = errors.New("servicerecipient: a recipient is x25519:<64 lowercase hex characters>")

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Parse validates a recipient and returns its raw 32 public-key bytes. It
// refuses an uppercase or otherwise re-spelled key: wrapped_keys compares
// recipients as strings, so two spellings of one key must not both exist.
func Parse(recipient string) ([]byte, error) {
	hexed, ok := strings.CutPrefix(recipient, Prefix)
	if !ok || hexed != strings.ToLower(hexed) {
		return nil, fmt.Errorf("%w: got %q", ErrMalformed, recipient)
	}
	raw, err := hex.DecodeString(hexed)
	if err != nil {
		return nil, fmt.Errorf("%w: got %q", ErrMalformed, recipient)
	}
	if _, err := ecdh.X25519().NewPublicKey(raw); err != nil {
		return nil, fmt.Errorf("%w: got %q", ErrMalformed, recipient)
	}
	zero := true
	for _, b := range raw {
		if b != 0 {
			zero = false
			break
		}
	}
	if zero {
		return nil, fmt.Errorf("%w: the all-zero key is not a recipient", ErrMalformed)
	}
	return raw, nil
}

// Fingerprint renders a recipient's short fingerprint: the first 80 bits of
// SHA-256(FingerprintLabel ‖ 0x00 ‖ public key), in RFC 4648 base32, grouped as
// four blocks of four ("PYJI XGNZ K7ZH XHEJ"). It fails on a malformed recipient.
func Fingerprint(recipient string) (string, error) {
	raw, err := Parse(recipient)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(FingerprintLabel))
	h.Write([]byte{0})
	h.Write(raw)
	enc := encoding.EncodeToString(h.Sum(nil)[:fingerprintBytes])
	groups := make([]string, 0, len(enc)/4)
	for i := 0; i < len(enc); i += 4 {
		groups = append(groups, enc[i:i+4])
	}
	return strings.Join(groups, " "), nil
}

// SameFingerprint compares a fingerprint as an operator typed it against the
// computed one, ignoring spaces, dashes and case.
func SameFingerprint(typed, computed string) bool {
	norm := func(s string) string {
		s = strings.ToUpper(s)
		return strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(s)
	}
	return typed != "" && norm(typed) == norm(computed)
}
