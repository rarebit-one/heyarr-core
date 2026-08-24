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

// CanonicalMIME maps a media type onto the CONSTANT this route will serve for
// it, or reports that there is none.
//
// # This is a security boundary, and the shape of it is the point
//
// internal/api/blobs serves application/octet-stream deliberately, so that a
// peer never "invites a browser to render content the catalog has not
// classified" — its words. This route exists to override that, which hands
// back exactly the risk that decision avoided: an asset whose sniffed type is
// text/html, served as text/html from the peer's own origin, is stored XSS
// against every other page that origin serves.
//
// The signature on a capability is not the protection it appears to be. It
// proves nobody altered the type in transit; it says nothing about where the
// type came from, which is a scanner looking at whatever a file turned out to
// be. A torrent can contain an .html file.
//
// # Why a table of constants rather than a validator
//
// The first version of this checked the type and passed the caller's own
// string through. That is correct and it is not PROVABLY correct: the value
// written into the response header is still the value that arrived in the
// URL, and neither a reader nor CodeQL can see the difference between a
// validator and a validator with a hole in it. CodeQL said so
// (go/reflected-xss, high) and declined to accept the check as a sanitiser.
//
// Returning a constant from a lookup ends the argument. Nothing a caller sends
// can reach a response header: either the type is one of these, and a literal
// from this file is written, or nothing is written at all.
//
// Image types are absent deliberately. SVG is scriptable, and separating the
// inert image formats from the live ones is a subtler job than this route
// needs to take on — a renderer plays audio and video.
func CanonicalMIME(mime string) (string, bool) {
	canonical, ok := servableMIME[strings.ToLower(strings.TrimSpace(mime))]
	return canonical, ok
}

// servableMIME is every media type this route will name in a response.
//
// The keys are what an Asset might carry, which is whatever the scanner
// decided; the values are literals. Adding a row is a deliberate act, which is
// the intended cost — an unknown type is served as octet-stream and plays on
// fewer devices, which is a bad afternoon, where the alternative failure is
// scripted content on the peer's origin.
var servableMIME = map[string]string{
	// Video.
	"video/mp4":               "video/mp4",
	"video/x-m4v":             "video/x-m4v",
	"video/quicktime":         "video/quicktime",
	"video/x-matroska":        "video/x-matroska",
	"video/x-mkv":             "video/x-mkv",
	"video/webm":              "video/webm",
	"video/avi":               "video/avi",
	"video/x-msvideo":         "video/x-msvideo",
	"video/mpeg":              "video/mpeg",
	"video/mp2t":              "video/mp2t",
	"video/vnd.dlna.mpeg-tts": "video/vnd.dlna.mpeg-tts",
	"video/x-ms-wmv":          "video/x-ms-wmv",
	"video/x-flv":             "video/x-flv",
	"video/3gpp":              "video/3gpp",
	"video/ogg":               "video/ogg",

	// Audio.
	"audio/mpeg":             "audio/mpeg",
	"audio/mp4":              "audio/mp4",
	"audio/x-m4a":            "audio/x-m4a",
	"audio/aac":              "audio/aac",
	"audio/flac":             "audio/flac",
	"audio/x-flac":           "audio/x-flac",
	"audio/ogg":              "audio/ogg",
	"audio/opus":             "audio/opus",
	"audio/wav":              "audio/wav",
	"audio/x-wav":            "audio/x-wav",
	"audio/aiff":             "audio/aiff",
	"audio/x-aiff":           "audio/x-aiff",
	"audio/x-ms-wma":         "audio/x-ms-wma",
	"audio/x-ac3":            "audio/x-ac3",
	"audio/vnd.dolby.dd-raw": "audio/vnd.dolby.dd-raw",
	"audio/vnd.dlna.adts":    "audio/vnd.dlna.adts",
	"audio/x-monkeys-audio":  "audio/x-monkeys-audio",
	"audio/3gpp":             "audio/3gpp",
}

// PlayableMIME reports whether this route will name a type in a response.
//
// A thin reading of CanonicalMIME, for the mint-time check, which wants the
// question rather than the answer.
func PlayableMIME(mime string) bool {
	_, ok := CanonicalMIME(mime)
	return ok
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
