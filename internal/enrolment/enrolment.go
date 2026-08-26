// Package enrolment is the authentication half of Milestone 8 (§40, ADR-0048).
//
// It pairs with internal/grant, and the split is the whole point. A grant
// AUTHORISES — it says a principal may do a thing until an instant. A cert here
// AUTHENTICATES — it says a device key belongs to a user — and it authorises
// NOTHING. Keeping the two apart is what makes a lost device survivable: a stale
// cert lets a device keep claiming to be its user, but opens no resource without
// a separate, short grant, so the exposure of a lost device is the ≤24h ageing
// of the grants it already held, not the library (ADR-0048).
//
// The trust flows one way and offline. A user identity is the root: its private
// key is the user's, recovery restores it (ADR-0022), and only its PUBLIC key
// is ever pinned at a peer (ADR-0032 — private keys never enter the server's
// data dir). A peer authenticates a device by verifying the cert against the
// user key it PINNED at enrolment, with no reach to the user and no server
// issuing the device anything — which is the acceptance sentence's first half.
//
// A cert proves the binding; it does not prove possession. That a caller holds
// the device's private key is a challenge the transport asks (ADR-0012's mTLS,
// or a session nonce), not something a cert can carry — a cert replayed by
// someone who copied it still names a device key they cannot sign with.
package enrolment

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// Version prefixes every cert payload, so a change to what is signed is a cert
// that fails to parse rather than one verified against a different reading of
// its fields.
const Version = 1

// CertLifetime is the default life of an enrolment cert: 90 days, renewable.
//
// A cert authenticates and authorises nothing, so its lifetime is an
// authentication-FRESHNESS question, not an exposure one (ADR-0048) — which is
// why it is long where a grant's is short. Ninety days re-proves a device
// quarterly without nagging; a lost device is contained by grant ageing (≤24h,
// grant.MaxTTL) and the membership denylist, not by waiting this out. Chosen
// 2026-08-26; a shorter value only trades freshness for friction.
const CertLifetime = 90 * 24 * time.Hour

// SkewMargin is subtracted from a cert's life at verification, the same rule and
// the same direction as grant.SkewMargin: clock skew only ever SHORTENS the
// honoured window, so it fails toward refusing a valid cert, never toward
// honouring an expired one (ADR-0048).
const SkewMargin = 5 * time.Minute

// CredentialSeparator joins the two halves of a device credential — the
// user-signed enrolment cert and a fresh possession proof — into the single
// value presented under the Device authorization scheme. A tilde is outside
// base64url's alphabet, so it cannot occur inside either half, and deviceauth
// splits the presented credential on it. It lives here, with the two halves it
// joins, so the client that assembles the value and the server that splits it
// name one constant rather than two spellings of the same byte.
const CredentialSeparator = "~"

// Reason is the machine-readable enum a refusal reports, asserted with equality
// because the neighbours share substrings (expired / not_yet_valid).
type Reason string

// The refusal reasons VerifyCert reports, and ReasonOK for an honoured cert.
const (
	ReasonOK           Reason = "ok"
	ReasonMalformed    Reason = "malformed"
	ReasonUnknownUser  Reason = "unknown_user"
	ReasonBadSignature Reason = "bad_signature"
	ReasonNotYetValid  Reason = "not_yet_valid"
	ReasonExpired      Reason = "expired"
)

// The sentinel errors this package refuses with. Distinct because they call for
// different actions: an expiry is the system working, a bad signature is a
// forgery or the wrong user's key, an unknown user is a key nobody pinned.
var (
	ErrMalformed    = errors.New("enrolment: malformed cert")
	ErrUnknownUser  = errors.New("enrolment: user is not enrolled")
	ErrBadSignature = errors.New("enrolment: cert signature does not verify")
	ErrNotYetValid  = errors.New("enrolment: cert is not yet valid")
	ErrExpired      = errors.New("enrolment: cert has expired")

	// ErrIncomplete is a cert missing its user, its device, or its validity
	// window. A cert that binds nothing authenticates nothing.
	ErrIncomplete = errors.New("enrolment: a binding is empty")
)

// ReasonForCert maps a VerifyCert error onto its enum value.
func ReasonForCert(err error) Reason {
	switch {
	case err == nil:
		return ReasonOK
	case errors.Is(err, ErrUnknownUser):
		return ReasonUnknownUser
	case errors.Is(err, ErrBadSignature):
		return ReasonBadSignature
	case errors.Is(err, ErrNotYetValid):
		return ReasonNotYetValid
	case errors.Is(err, ErrExpired):
		return ReasonExpired
	default:
		return ReasonMalformed
	}
}

// A UserIdentity is the root of a user's authority: one Ed25519 keypair whose
// public half is pinned at peers and whose private half signs enrolment certs
// (here) and capability grants (internal/grant). One user, many devices (§40).
//
// It follows internal/peer/identity.Identity: there is no private-key field, so
// a UserIdentity handed to a log, a --json document or an MCP result cannot leak
// the seed. Signing is a separate, explicit act through Signer.
type UserIdentity struct {
	PublicKey ed25519.PublicKey
}

// UserID renders the identity the way a peer key is rendered (ADR-0012, #135):
// algorithm-prefixed lowercase hex. It is the principal's stable name, the cert's
// authority, and a grant issuer id, so one string threads all three.
func (u UserIdentity) UserID() string { return identity.FormatPublicKey(u.PublicKey) }

// GenerateUserIdentity creates a new user identity and returns it with its
// private key. The caller writes the seed to the USER's config, never a server
// data dir (ADR-0032), and hands only the UserIdentity to anything that renders.
func GenerateUserIdentity() (UserIdentity, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return UserIdentity{}, nil, fmt.Errorf("enrolment: generating a user identity: %w", err)
	}
	return UserIdentity{PublicKey: pub}, priv, nil
}

// A Cert is a user's signed statement that a device key is theirs, for a window.
//
// User and Device are rendered public keys ("ed25519:<hex>"). User is the
// authority a verifier checks the signature against; Device is who the cert
// authenticates. It carries no capability — that is a grant's job.
type Cert struct {
	User      string
	Device    string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

type payload struct {
	V        int    `json:"v"`
	User     string `json:"usr"`
	Device   string `json:"dev"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

// SignCert issues a cert binding devicePub to the signer, valid for lifetime
// from issuedAt. A zero lifetime uses CertLifetime.
//
// It refuses to mint a cert that could not be honoured — no device, a lifetime
// that does not advance — for the same reason grant.Sign does: a cert that
// exists should be one that could have authenticated, so its refusal at
// verification cannot be mistaken for an attack.
func SignCert(userPriv ed25519.PrivateKey, devicePub ed25519.PublicKey, issuedAt time.Time, lifetime time.Duration) (string, error) {
	if len(userPriv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("enrolment: a user signing key is required")
	}
	if len(devicePub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("%w: a device public key is required", ErrIncomplete)
	}
	if issuedAt.IsZero() {
		return "", fmt.Errorf("%w: an issued-at is required", ErrIncomplete)
	}
	if lifetime <= 0 {
		lifetime = CertLifetime
	}
	user := identity.FormatPublicKey(userPriv.Public().(ed25519.PublicKey))
	body, err := json.Marshal(payload{
		V:        Version,
		User:     user,
		Device:   identity.FormatPublicKey(devicePub),
		IssuedAt: issuedAt.Unix(),
		Expires:  issuedAt.Add(lifetime).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("enrolment: encoding: %w", err)
	}
	sig := ed25519.Sign(userPriv, body)
	return encode(body) + "." + encode(sig), nil
}

// VerifyCert authenticates a cert against the user key a peer has PINNED, and a
// clock. It returns the cert — whose Device is the authenticated device key — or
// the reason it is refused.
//
// pinnedUser is the caller's membership answer to "is this user enrolled here".
// The enrol-before-trust gate (ADR-0032) is the stated-user match below: a cert
// naming a user this verifier has not pinned is refused ErrUnknownUser, and the
// signature must then verify against that pinned key, so a cert cannot name one
// user and be signed by another. The length check here is defence in depth, not
// that gate — it refuses a malformed pinned key early and, load-bearingly, keeps
// one away from ed25519.Verify, which PANICS on a wrong-length key rather than
// returning false. (A nil pinned key is also caught by the match below, since
// FormatPublicKey(nil) is "" and no real cert names the empty user — so this
// guard is redundant for correctness and kept only for the panic and the reason.)
//
// Order mirrors grant.Verify: signature before fields, temporal validity last,
// and the skew margin only shortens the honoured window.
func VerifyCert(token string, pinnedUser ed25519.PublicKey, now time.Time) (Cert, error) {
	if len(pinnedUser) != ed25519.PublicKeySize {
		return Cert{}, fmt.Errorf("%w: no pinned key supplied", ErrUnknownUser)
	}
	bodyEnc, sigEnc, ok := strings.Cut(token, ".")
	if !ok {
		return Cert{}, ErrMalformed
	}
	body, err := decode(bodyEnc)
	if err != nil {
		return Cert{}, ErrMalformed
	}
	sig, err := decode(sigEnc)
	if err != nil {
		return Cert{}, ErrMalformed
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Cert{}, ErrMalformed
	}
	if p.V != Version || p.User == "" || p.Device == "" {
		return Cert{}, ErrMalformed
	}

	// The cert must name the pinned user, and be signed by them. A cert naming a
	// different user than the one the caller pinned is refused as unenrolled;
	// one that names the right user but is signed by another key fails the
	// signature (nobody can forge it).
	if p.User != identity.FormatPublicKey(pinnedUser) {
		return Cert{}, fmt.Errorf("%w: cert names %s", ErrUnknownUser, p.User)
	}
	if !ed25519.Verify(pinnedUser, body, sig) {
		return Cert{}, ErrBadSignature
	}

	c := Cert{
		User:      p.User,
		Device:    p.Device,
		IssuedAt:  time.Unix(p.IssuedAt, 0).UTC(),
		ExpiresAt: time.Unix(p.Expires, 0).UTC(),
	}
	if now.Add(SkewMargin).Before(c.IssuedAt) {
		return Cert{}, fmt.Errorf("%w: valid from %s, now %s", ErrNotYetValid, c.IssuedAt, now)
	}
	if !now.Before(c.ExpiresAt.Add(-SkewMargin)) {
		return Cert{}, fmt.Errorf("%w: expired at %s, now %s", ErrExpired, c.ExpiresAt, now)
	}
	return c, nil
}

// CertUser reads the user a cert CLAIMS, without verifying it. It is the hint a
// caller uses to look up which pinned key to check the cert against — the exact
// role the issuer field plays in a grant. Reading it proves nothing: VerifyCert
// against the looked-up key is what makes it true, and a cert naming a user the
// caller has not pinned is refused there.
func CertUser(token string) (string, error) {
	bodyEnc, _, ok := strings.Cut(token, ".")
	if !ok {
		return "", ErrMalformed
	}
	body, err := decode(bodyEnc)
	if err != nil {
		return "", ErrMalformed
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return "", ErrMalformed
	}
	if p.V != Version || p.User == "" {
		return "", ErrMalformed
	}
	return p.User, nil
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
