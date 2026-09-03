package resources

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

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// The stream token (ADR-0069).
//
// A plan that answers `stream` hands back a URL whose last segment is this
// token. It is signed, not stored: the node keeps no table of live plans, and a
// token that verifies IS the plan — which blob, what the repackager must do to
// it, who may fetch it, and until when. Everything the stream route needs is
// in the token, so the route does no planning of its own and cannot be talked
// into a different repackage than the one that was planned.
//
// # Bound to the credential that planned it
//
// The render capability (ADR-0040) is deliberately unbound: it is for a
// television that has no credential. This one is the opposite: the stream
// route sits behind the same middleware as everything else, and the token
// additionally names the credential that asked for the plan. A token lifted
// out of one client's logs is useless to a caller holding any other
// credential, which closes the one gap the playback token's "not scoped to one
// blob" note in playback.go leaves open — a stream token is scoped to one
// blob AND one caller.
//
// # Signed with a key derived from the render secret
//
// One secret on disk, two keys: the render key signs capabilities and this key
// signs stream tokens, derived by HMAC over a fixed label so a token minted by
// one route can never verify at the other even though both live behind the
// same file.

// streamTokenVersion prefixes every token, so a change to what is signed is a
// token that fails to parse rather than one that verifies against the wrong
// reading of its fields.
const streamTokenVersion = "s1"

// streamTokenTTL is how long a stream token may be presented for.
//
// Shorter than the two-hour playback credential and deliberately so: the
// token names a repackage, and a repackage is planned against what the client
// says it can decode NOW. An hour covers a film's worth of reconnects — a
// player that drops and re-fetches the same URL re-presents the same token —
// and a client that outlives it re-plans, which is one request. The stream
// itself is not cut off at expiry; only its START is.
const streamTokenTTL = time.Hour

var (
	errStreamTokenMalformed = errors.New("stream token: malformed")
	errStreamTokenSignature = errors.New("stream token: signature does not verify")
	errStreamTokenExpired   = errors.New("stream token: expired")
	errStreamTokenSubject   = errors.New("stream token: presented by a different credential than planned it")
)

// streamToken is what a stream URL permits.
type streamToken struct {
	BlobHash string
	// Subject is the credential the plan was minted for — see streamSubject.
	Subject string
	// CopyVideo, CopyAudio and MaxHeight are the repackage, as the domain
	// decided it. Signed, so a client cannot upgrade a copy to a transcode.
	CopyVideo bool
	CopyAudio bool
	MaxHeight int
	ExpiresAt time.Time
}

// streamSubject names the credential a request arrived with, in a form that
// is stable across requests by the same credential and distinct across
// credentials.
//
// A bearer token has an id; a device credential has none (it is a cert and a
// fresh proof each time) and is named by its key; the anonymous identity a
// loopback deployment runs under is named as such. The principal id is
// prefixed so a device key and a token id can never collide by accident.
func streamSubject(id auth.Identity) string {
	switch {
	case id.Anonymous:
		return "anonymous"
	case id.Token.ID != "":
		return id.Principal.ID + "/token:" + id.Token.ID
	default:
		return id.Principal.ID + "/" + id.Token.Name
	}
}

// streamKey derives the signing key from the node's secret.
func streamKey(secret []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("heyarr playback stream token " + streamTokenVersion))
	return mac.Sum(nil)
}

// newStreamSecret mints a per-process secret for a node with none configured.
// Its tokens survive only this process, which is fine for something that
// lives an hour and is one request to re-mint.
func newStreamSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("stream token: generating a secret: %w", err)
	}
	return b, nil
}

func (t streamToken) payload() string {
	flags := 0
	if t.CopyVideo {
		flags |= 1
	}
	if t.CopyAudio {
		flags |= 2
	}
	return strings.Join([]string{
		streamTokenVersion,
		b64([]byte(t.BlobHash)),
		b64([]byte(t.Subject)),
		strconv.Itoa(flags),
		strconv.Itoa(t.MaxHeight),
		strconv.FormatInt(t.ExpiresAt.Unix(), 10),
	}, ".")
}

// sign renders the token. Every field is base64url-encoded before joining so
// no field can carry the delimiter, and the signature covers all of them.
func (t streamToken) sign(key []byte) (string, error) {
	if len(key) == 0 {
		return "", errors.New("stream token: a signing key is required")
	}
	if t.BlobHash == "" || t.Subject == "" || t.ExpiresAt.IsZero() {
		return "", errors.New("stream token: a blob, a subject and an expiry are required")
	}
	p := t.payload()
	return p + "." + b64(streamMAC(key, p)), nil
}

// verifyStreamToken checks a token and returns what it permits: signature
// first, then shape, then expiry, then the subject — reading any field before
// the signature has been checked is trusting a field nobody authenticated.
func verifyStreamToken(key []byte, token, subject string, now time.Time) (streamToken, error) {
	if len(key) == 0 {
		return streamToken{}, errors.New("stream token: a signing key is required")
	}
	cut := strings.LastIndex(token, ".")
	if cut < 0 {
		return streamToken{}, errStreamTokenMalformed
	}
	payload, signature := token[:cut], token[cut+1:]
	got, err := unb64(signature)
	if err != nil {
		return streamToken{}, errStreamTokenMalformed
	}
	if subtle.ConstantTimeCompare(streamMAC(key, payload), got) != 1 {
		return streamToken{}, errStreamTokenSignature
	}

	fields := strings.Split(payload, ".")
	if len(fields) != 6 || fields[0] != streamTokenVersion {
		return streamToken{}, errStreamTokenMalformed
	}
	blob, err := unb64(fields[1])
	if err != nil {
		return streamToken{}, errStreamTokenMalformed
	}
	subj, err := unb64(fields[2])
	if err != nil {
		return streamToken{}, errStreamTokenMalformed
	}
	flags, err := strconv.Atoi(fields[3])
	if err != nil || flags < 0 || flags > 3 {
		return streamToken{}, errStreamTokenMalformed
	}
	height, err := strconv.Atoi(fields[4])
	if err != nil || height < 0 {
		return streamToken{}, errStreamTokenMalformed
	}
	unix, err := strconv.ParseInt(fields[5], 10, 64)
	if err != nil {
		return streamToken{}, errStreamTokenMalformed
	}
	t := streamToken{
		BlobHash:  string(blob),
		Subject:   string(subj),
		CopyVideo: flags&1 != 0,
		CopyAudio: flags&2 != 0,
		MaxHeight: height,
		ExpiresAt: time.Unix(unix, 0).UTC(),
	}
	if !now.Before(t.ExpiresAt) {
		return streamToken{}, errStreamTokenExpired
	}
	if subtle.ConstantTimeCompare([]byte(t.Subject), []byte(subject)) != 1 {
		return streamToken{}, errStreamTokenSubject
	}
	return t, nil
}

func streamMAC(key []byte, payload string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
