// Package render serves bytes to devices that can only fetch a URL (ADR-0039).
//
// A UPnP renderer, an AirPlay receiver or a television's built-in player is
// handed a URL and told to fetch it. That is the whole of its vocabulary: no
// Authorization header, no JSON plan, no retry with a credential. Heyarr's
// authenticated blob endpoint answers such a device with 401, correctly and
// permanently — internal/api/http/auth.go refuses query credentials on purpose.
//
// So this is a separate mount with a separate trust root, exactly as
// internal/api/blobs describes itself: "one function, two mount points, two
// trust roots". This is the third. The blob contract is untouched, which is
// what ADR-0013 asks for; what changes is that there is now a route where the
// URL itself is the authority.
package render

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// capabilityVersion prefixes every token.
//
// It is here so that a change to what is signed is a token that fails to parse
// rather than one that verifies against the wrong interpretation of its own
// fields. Tokens are short-lived, so a version bump costs one expiry window.
const capabilityVersion = "v1"

// SecretLen is the length of a signing secret.
const SecretLen = 32

var (
	// ErrMalformed is a token that is not a capability at all.
	ErrMalformed = errors.New("render: malformed capability")
	// ErrSignature is a token whose signature does not verify — a forgery, a
	// truncation, or a token minted by a different peer. ADR-0039 scopes a
	// capability to the node that minted it, so the third is a real case and
	// not an attack.
	ErrSignature = errors.New("render: capability signature does not verify")
	// ErrExpired is a well-formed, correctly signed, out-of-date token.
	//
	// It is distinct from ErrSignature because the two mean opposite things to
	// an operator: expired is the system working, and a bad signature is
	// somebody's television being handed a token from the wrong peer — or
	// something worth looking at.
	ErrExpired = errors.New("render: capability has expired")
)

// Capability is permission to fetch one blob, until one instant, as one type.
//
// It carries no identity. That is the point: a renderer has none to present,
// and inventing one for it would be the "second authorisation model" that
// internal/api/resources/playback.go refuses to build ahead of §77's grants.
type Capability struct {
	// BlobHash is the blob this token permits, and only this blob. A leaked
	// capability discloses one film, not the library — which is the whole of
	// why it is a capability and not a scoped credential.
	BlobHash string
	// ExpiresAt bounds the damage a leak can do.
	ExpiresAt time.Time
	// MIME is the Content-Type to serve, decided when an Asset was in hand and
	// signed so nobody can change it in transit.
	//
	// It is part of the capability rather than something the handler works out
	// because a blob has no type — bytes are identity (ADR-0006) — and the
	// endpoint that serves them must not learn about Assets to find one.
	MIME string
}

// NewSecret generates a signing secret.
func NewSecret() ([]byte, error) {
	secret := make([]byte, SecretLen)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("render: generating a signing secret: %w", err)
	}
	return secret, nil
}

// Sign renders a capability as a URL-safe token.
//
// The signature covers every field. A token pointed at a different blob, given
// a longer life or relabelled with a type the device will accept is a token
// that fails to verify, so none of the three has to be re-checked by the
// handler.
func (c Capability) Sign(secret []byte) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("render: a signing secret is required")
	}
	if c.BlobHash == "" {
		return "", errors.New("render: a capability needs a blob")
	}
	if c.ExpiresAt.IsZero() {
		return "", errors.New("render: a capability needs an expiry")
	}
	// Every field is base64url-encoded before it is joined, so no field can
	// contain the delimiter and two different capabilities cannot share one
	// signed string. That encoding is load-bearing rather than cosmetic: real
	// MIME types contain dots — a Samsung declares audio/vnd.dolby.dd-raw —
	// and a format that joined them raw would be ambiguous on exactly those.

	payload := c.payload()
	return payload + "." + encode(sign(secret, payload)), nil
}

func (c Capability) payload() string {
	return strings.Join([]string{
		capabilityVersion,
		encode([]byte(c.BlobHash)),
		strconv.FormatInt(c.ExpiresAt.Unix(), 10),
		encode([]byte(c.MIME)),
	}, ".")
}

// Verify checks a token and returns what it permits.
//
// The order is deliberate: signature before expiry. Reading an expiry out of an
// unverified token and acting on it — even to reject it — is trusting a field
// nobody has authenticated yet.
func Verify(secret []byte, token string, now time.Time) (Capability, error) {
	if len(secret) == 0 {
		return Capability{}, errors.New("render: a signing secret is required")
	}
	cut := strings.LastIndex(token, ".")
	if cut < 0 {
		return Capability{}, ErrMalformed
	}
	payload, signature := token[:cut], token[cut+1:]

	want := sign(secret, payload)
	got, err := decode(signature)
	if err != nil {
		return Capability{}, ErrMalformed
	}
	// Constant time, because the comparison is against a value an attacker
	// supplies and can vary a byte at a time.
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return Capability{}, ErrSignature
	}

	fields := strings.Split(payload, ".")
	if len(fields) != 4 || fields[0] != capabilityVersion {
		return Capability{}, ErrMalformed
	}
	blob, err := decode(fields[1])
	if err != nil {
		return Capability{}, ErrMalformed
	}
	unix, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return Capability{}, ErrMalformed
	}
	mime, err := decode(fields[3])
	if err != nil {
		return Capability{}, ErrMalformed
	}

	c := Capability{
		BlobHash:  string(blob),
		ExpiresAt: time.Unix(unix, 0),
		MIME:      string(mime),
	}
	if !now.Before(c.ExpiresAt) {
		return Capability{}, ErrExpired
	}
	return c, nil
}

func sign(secret []byte, payload string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
