package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
)

// TokenPrefix marks a Heyarr credential in a log, an environment file or a
// secret scanner. Secret scanning only works on credentials that announce
// themselves, and a leaked token nobody can recognise is a leaked token nobody
// revokes.
const TokenPrefix = "heyarr_"

// secretBytes is the entropy in the secret half of a token. 256 bits: brute
// force is not a threat model, it is arithmetic.
const secretBytes = 32

// b32 is lowercase RFC 4648 base32 without padding. Base32 rather than base64
// because tokens get copied out of terminals, typed into config files and read
// aloud, and base64's case sensitivity plus `+/=` makes all three worse.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func encode32(b []byte) string { return strings.ToLower(b32.EncodeToString(b)) }
func decode32(s string) ([]byte, error) {
	return b32.DecodeString(strings.ToUpper(s))
}

// ErrMalformedToken means the presented credential is not a Heyarr token at
// all. It is reported to the caller as an ordinary 401: telling an unauthorised
// client *why* its credential was rejected is free help.
var ErrMalformedToken = errors.New("auth: malformed token")

// Presented is a bearer token as parsed from a request.
//
// A token is `heyarr_<id>_<secret>` — a public selector and a secret verifier.
// The selector is the api_tokens row id, which is what makes verification O(1)
// instead of "argon2-verify the presented secret against every token in the
// table" — at any sane argon2 cost that is not a slow lookup, it is a denial of
// service against yourself. The selector is not secret and grants nothing: it
// is the same id `heyarr token list` prints and `heyarr token revoke` takes.
type Presented struct {
	// ID is the api_tokens row id (a UUIDv7 string).
	ID string
	// Secret is the 256-bit verifier.
	Secret []byte
	// Raw is the full token as presented, used only as a cache key. It is
	// never logged and never stored.
	Raw string
}

// ParseToken splits a presented credential without touching the database.
//
// Both halves must be *canonically* encoded, not merely decodable. Unpadded
// base32 does not use every bit of its final character — 32 bytes is 52
// characters with four bits spare — so several distinct strings decode to the
// same secret. Accepting all of them would mean one credential has many
// spellings: the verified-token cache would key each spelling separately, and
// any future rate limit or audit keyed on the presented string would count them
// as different tokens. Re-encoding and comparing costs nothing and leaves
// exactly one spelling per token.
func ParseToken(raw string) (Presented, error) {
	if !strings.HasPrefix(raw, TokenPrefix) {
		return Presented{}, ErrMalformedToken
	}
	body := strings.TrimPrefix(raw, TokenPrefix)
	idPart, secretPart, ok := strings.Cut(body, "_")
	if !ok {
		return Presented{}, ErrMalformedToken
	}
	idBytes, err := decode32(idPart)
	if err != nil || len(idBytes) != 16 || encode32(idBytes) != idPart {
		return Presented{}, ErrMalformedToken
	}
	id, err := uuid.FromBytes(idBytes)
	if err != nil {
		return Presented{}, ErrMalformedToken
	}
	secret, err := decode32(secretPart)
	if err != nil || len(secret) != secretBytes || encode32(secret) != secretPart {
		return Presented{}, ErrMalformedToken
	}
	return Presented{ID: id.String(), Secret: secret, Raw: raw}, nil
}

// NewToken mints a credential: a fresh UUIDv7 row id and 256 bits of secret.
// The returned string is the only time the secret exists in plaintext.
func NewToken() (id string, raw string, secret []byte, err error) {
	u, err := uuid.NewV7()
	if err != nil {
		return "", "", nil, fmt.Errorf("auth: generating token id: %w", err)
	}
	secret = make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", "", nil, fmt.Errorf("auth: generating token secret: %w", err)
	}
	idBytes := u
	return u.String(), TokenPrefix + encode32(idBytes[:]) + "_" + encode32(secret), secret, nil
}

// Argon2 parameters (RFC 9106 §4, the memory-constrained recommendation).
//
// These are deliberately *not* cheap, and that costs real latency: one verify
// is tens of milliseconds and allocates 64 MiB. Running it on every request
// would make the API unusably slow — and dropping the parameters until it felt
// fast would quietly convert the at-rest protection into decoration. The
// resolution is the verified-token cache in Verifier, not weaker parameters.
//
// The parameters are stored *in* the hash string, so raising them later is a
// configuration change plus a rehash-on-next-use, not a migration.
const (
	argonTime    uint32 = 2
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// HashSecret returns a PHC-format argon2id hash of a token secret.
func HashSecret(secret []byte) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generating salt: %w", err)
	}
	key := argon2.IDKey(secret, salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifySecret reports whether secret produced the stored hash.
//
// The comparison is constant-time. The window is narrow — an attacker would
// need to already hold a valid selector — but a timing-variable compare on a
// credential check is the kind of thing that is free to get right now and
// expensive to notice later.
func VerifySecret(encoded string, secret []byte) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, fmt.Errorf("auth: stored hash is not argon2id")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, fmt.Errorf("auth: stored hash has no version: %w", err)
	}
	if version != argon2.Version {
		return false, fmt.Errorf("auth: stored hash uses argon2 version %d, this build understands %d",
			version, argon2.Version)
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, fmt.Errorf("auth: stored hash has unreadable parameters: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("auth: stored hash has an unreadable salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("auth: stored hash is unreadable: %w", err)
	}
	// #nosec G115 -- len(want) is bounded by the stored hash we just decoded.
	got := argon2.IDKey(secret, salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// cacheKey is the lookup key for the verified-token cache: a fast digest of
// the presented credential, so the cache never holds the token itself.
func cacheKey(raw string) [sha256.Size]byte { return sha256.Sum256([]byte(raw)) }
