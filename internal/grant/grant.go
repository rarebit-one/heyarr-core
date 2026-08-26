// Package grant is the signed, expiring delegation of ADR-0048.
//
// A grant is Milestone 8's answer to the one thing ADR-0038 could not resolve:
// a capability that spans sites. Under the peer-repo model there is no authority
// to ask whether a grant still holds, so the grant carries its own bound — it is
// signed, and it expires, and a peer honours it against a key it pinned at
// enrolment and a clock of its own, never a live reach to the issuer.
//
// It is deliberately NOT internal/api/render's capability. That one is an
// HMAC — symmetric, and so "only valid at the peer that minted it" (ADR-0040),
// which is exactly what a cross-site grant cannot be. This one is Ed25519:
// the verifier holds the issuer's PUBLIC key, so a peer that has never met the
// issuer's secret, and cannot reach the issuer, still verifies the grant. The
// two coexist as separate trust roots — HMAC for the television that has no
// identity, this for the device that does.
//
// A grant is not a bearer token with extra fields (ADR-0038, #285): it binds a
// principal, a resource, a capability set and an expiry, all four are inside the
// signed payload, and Verify proves each one constrains by refusing with a
// DISTINCT, named reason when it does not. The reasons share words on purpose
// (expired / not_yet_valid, principal_ / resource_mismatch), so a caller asserts
// them with equality, never with a substring.
package grant

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// Version prefixes every payload. A change to what is signed is then a grant
// that fails to parse rather than one that verifies against a different reading
// of its own fields — grants are short-lived, so a bump costs one TTL.
const Version = 1

// MaxTTL is the longest life a grant may be signed with (ADR-0048).
//
// It is the revocation window: a grant revoked at one site is honoured at an
// unreachable site for at most its remaining life, and this caps that life at
// 24 hours. Enforced at Sign, so a grant that outlives the window cannot be
// minted rather than merely being refused later. Steady-state revocation is far
// faster — grants are re-issued on a short cadence while the peers can reach
// each other — but 24h is the bound when they cannot.
const MaxTTL = 24 * time.Hour

// SkewMargin is subtracted from a grant's life at verification (ADR-0048).
//
// Clock skew is a security parameter here, and it must fail toward refusing a
// valid grant, never toward honouring an expired one. So the margin only ever
// SHORTENS the honoured window: a verifier whose clock is correct refuses
// SkewMargin early, and one whose clock is behind by up to SkewMargin still
// refuses no later than the true expiry. A clock behind by MORE than this
// over-honours by exactly the excess — bounded, and small against MaxTTL on
// NTP-synced hosts, which is why the TTL and not the clock is the guarantee.
const SkewMargin = 5 * time.Minute

// A Capability is one verb a grant may carry. It is a distinct type so a bare
// string cannot be passed where a checked capability is meant, and so read can
// never be silently widened into write.
type Capability string

// The capabilities this milestone defines. read is a peer's degraded-read
// authority (§53, §54); write is named so a test can prove a read grant does
// not imply it.
const (
	CapabilityRead  Capability = "read"
	CapabilityWrite Capability = "write"
)

// Reason is the machine-readable enum a refusal reports. Every value is a word
// a caller asserts with equality (ADR-0048): the neighbours share substrings, so
// a substring assertion would pass on the wrong one.
type Reason string

// The refusal reasons Verify reports, and ReasonOK for a honoured grant.
const (
	ReasonOK                Reason = "ok"
	ReasonMalformed         Reason = "malformed"
	ReasonUnknownIssuer     Reason = "unknown_issuer"
	ReasonBadSignature      Reason = "bad_signature"
	ReasonPrincipalMismatch Reason = "principal_mismatch"
	ReasonResourceMismatch  Reason = "resource_mismatch"
	ReasonCapabilityDenied  Reason = "capability_denied"
	ReasonNotYetValid       Reason = "not_yet_valid"
	ReasonExpired           Reason = "expired"
)

// The sentinel errors Verify and Sign refuse with. They are distinct because
// they call for different actions: an expiry is the system working, a bad
// signature is a forgery or a grant from the wrong issuer, an unknown issuer is
// a key nobody enrolled. A test that asserted "some error" would pass on the
// wrong one, which is the failure #285's acceptance calls out by name.
var (
	ErrMalformed         = errors.New("grant: malformed")
	ErrUnknownIssuer     = errors.New("grant: issuer is not enrolled")
	ErrBadSignature      = errors.New("grant: signature does not verify")
	ErrPrincipalMismatch = errors.New("grant: grant is for a different principal")
	ErrResourceMismatch  = errors.New("grant: grant is for a different resource")
	ErrCapabilityDenied  = errors.New("grant: grant does not carry this capability")
	ErrNotYetValid       = errors.New("grant: grant is not yet valid")
	ErrExpired           = errors.New("grant: grant has expired")

	// ErrTTLTooLong is a grant asked to outlive MaxTTL. It fires at Sign, so the
	// revocation window cannot be widened by minting rather than only by an
	// operator changing a constant.
	ErrTTLTooLong = errors.New("grant: ttl exceeds the maximum")
	// ErrIncomplete is a grant missing a binding — no principal, no resource, no
	// capability. A grant that binds nothing is the bearer token ADR-0048 refuses.
	ErrIncomplete = errors.New("grant: a binding is empty")
	// ErrIssuerMismatch is a grant whose stated issuer is not the key signing it.
	ErrIssuerMismatch = errors.New("grant: issuer does not match the signing key")
)

// ReasonFor maps a Verify error onto its enum value, so a caller reports the
// refusal without re-deriving it from the sentinel.
func ReasonFor(err error) Reason {
	switch {
	case err == nil:
		return ReasonOK
	case errors.Is(err, ErrUnknownIssuer):
		return ReasonUnknownIssuer
	case errors.Is(err, ErrBadSignature):
		return ReasonBadSignature
	case errors.Is(err, ErrPrincipalMismatch):
		return ReasonPrincipalMismatch
	case errors.Is(err, ErrResourceMismatch):
		return ReasonResourceMismatch
	case errors.Is(err, ErrCapabilityDenied):
		return ReasonCapabilityDenied
	case errors.Is(err, ErrNotYetValid):
		return ReasonNotYetValid
	case errors.Is(err, ErrExpired):
		return ReasonExpired
	default:
		return ReasonMalformed
	}
}

// A Grant is permission for one principal to exercise a capability set on one
// resource, from one instant until another, on the authority of one issuer.
//
// Issuer is the issuer's public key in identity's rendered form
// ("ed25519:<hex>"), so a verifier can both find the pinned key to check against
// and see, in a log, whose authority a grant claims.
type Grant struct {
	Issuer       string
	Principal    string
	Resource     string
	Capabilities []Capability
	IssuedAt     time.Time
	ExpiresAt    time.Time
}

// payload is the on-the-wire, signed form. Times are unix seconds — a grant is
// coarse-grained and second precision keeps the encoding stable across the JSON
// round trip. Capabilities are sorted and de-duplicated at Sign so the signed
// bytes are deterministic for a given set.
type payload struct {
	V            int      `json:"v"`
	Issuer       string   `json:"iss"`
	Principal    string   `json:"prn"`
	Resource     string   `json:"res"`
	Capabilities []string `json:"cap"`
	IssuedAt     int64    `json:"iat"`
	ExpiresAt    int64    `json:"exp"`
}

// A Request is what a caller is trying to do, checked against a grant.
type Request struct {
	Principal  string
	Resource   string
	Capability Capability
}

// TrustStore resolves an issuer to the public key a peer has pinned for it
// (ADR-0012, extended to user identities in ADR-0048). A grant whose issuer the
// store does not know is refused ErrUnknownIssuer — this is the gate that makes
// "a key issued and immediately honoured" (ADR-0032) unspellable: an issuer must
// be enrolled, out of band, BEFORE any grant naming it is honoured.
type TrustStore interface {
	// IssuerKey returns the pinned public key for issuer, and whether it is
	// enrolled at all.
	IssuerKey(issuer string) (ed25519.PublicKey, bool)
}

// Keys is a TrustStore backed by a map, keyed by rendered public key. It is the
// simplest possible pinned set, and enough for a verifier that already holds its
// membership in memory.
type Keys map[string]ed25519.PublicKey

// IssuerKey implements TrustStore.
func (k Keys) IssuerKey(issuer string) (ed25519.PublicKey, bool) {
	pub, ok := k[issuer]
	return pub, ok
}

// Enrol adds pub to the set under its rendered issuer id, and returns that id.
func (k Keys) Enrol(pub ed25519.PublicKey) string {
	id := identity.FormatPublicKey(pub)
	k[id] = pub
	return id
}

// Sign renders a grant as a URL-safe token, signed by priv.
//
// It refuses to mint a grant that is not honourable: an empty binding
// (ErrIncomplete), a life past MaxTTL (ErrTTLTooLong), or a stated issuer that
// is not priv's own public key (ErrIssuerMismatch). Catching these here means a
// grant that exists is a grant that could have been honoured — the alternative,
// discovering it at Verify, is a refusal an operator cannot tell from an attack.
func (g Grant) Sign(priv ed25519.PrivateKey) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("grant: a signing key is required")
	}
	self := identity.FormatPublicKey(priv.Public().(ed25519.PublicKey))
	if g.Issuer != self {
		return "", fmt.Errorf("%w: grant names %q, key is %q", ErrIssuerMismatch, g.Issuer, self)
	}
	if g.Principal == "" || g.Resource == "" || len(g.Capabilities) == 0 {
		return "", ErrIncomplete
	}
	if g.IssuedAt.IsZero() || g.ExpiresAt.IsZero() {
		return "", fmt.Errorf("%w: a grant needs an issued-at and an expiry", ErrIncomplete)
	}
	if !g.ExpiresAt.After(g.IssuedAt) {
		return "", fmt.Errorf("%w: expiry is not after issue", ErrIncomplete)
	}
	if g.ExpiresAt.Sub(g.IssuedAt) > MaxTTL {
		return "", fmt.Errorf("%w: %s > %s", ErrTTLTooLong, g.ExpiresAt.Sub(g.IssuedAt), MaxTTL)
	}

	body, err := json.Marshal(payload{
		V:            Version,
		Issuer:       g.Issuer,
		Principal:    g.Principal,
		Resource:     g.Resource,
		Capabilities: canonicalCaps(g.Capabilities),
		IssuedAt:     g.IssuedAt.Unix(),
		ExpiresAt:    g.ExpiresAt.Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("grant: encoding: %w", err)
	}
	sig := ed25519.Sign(priv, body)
	return encode(body) + "." + encode(sig), nil
}

// Verify checks a token against a request, a trust store and a clock, and
// returns the grant it authorises or the reason it does not.
//
// The order is deliberate. The issuer field is read first — but only to SELECT
// which pinned key to check against, never as authority: a token pointed at a
// different enrolled issuer fails the signature (nobody can forge it), and one
// pointed at an unknown issuer is refused before any field is trusted. Only
// after the signature verifies are the four bindings and the clock read, because
// until then every field is a claim.
//
// Each binding is checked separately so its refusal is its own reason. Temporal
// validity is checked LAST, so a grant that matches the request but has expired
// reports expired — the most useful thing to tell an operator — rather than a
// mismatch it does not have.
func Verify(token string, store TrustStore, req Request, now time.Time) (Grant, error) {
	if store == nil {
		return Grant{}, fmt.Errorf("grant: a trust store is required")
	}
	bodyEnc, sigEnc, ok := strings.Cut(token, ".")
	if !ok {
		return Grant{}, ErrMalformed
	}
	body, err := decode(bodyEnc)
	if err != nil {
		return Grant{}, ErrMalformed
	}
	sig, err := decode(sigEnc)
	if err != nil {
		return Grant{}, ErrMalformed
	}

	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Grant{}, ErrMalformed
	}
	if p.V != Version || p.Issuer == "" {
		return Grant{}, ErrMalformed
	}

	// Select the pinned key by the issuer HINT, then let the signature prove it.
	pub, enrolled := store.IssuerKey(p.Issuer)
	if !enrolled {
		return Grant{}, fmt.Errorf("%w: %s", ErrUnknownIssuer, p.Issuer)
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, body, sig) {
		return Grant{}, ErrBadSignature
	}

	// Authenticated from here down.
	g := Grant{
		Issuer:       p.Issuer,
		Principal:    p.Principal,
		Resource:     p.Resource,
		Capabilities: toCaps(p.Capabilities),
		IssuedAt:     time.Unix(p.IssuedAt, 0).UTC(),
		ExpiresAt:    time.Unix(p.ExpiresAt, 0).UTC(),
	}

	if req.Principal == "" || g.Principal != req.Principal {
		return Grant{}, fmt.Errorf("%w: grant %q, request %q", ErrPrincipalMismatch, g.Principal, req.Principal)
	}
	if req.Resource == "" || g.Resource != req.Resource {
		return Grant{}, fmt.Errorf("%w: grant %q, request %q", ErrResourceMismatch, g.Resource, req.Resource)
	}
	if !hasCapability(g.Capabilities, req.Capability) {
		return Grant{}, fmt.Errorf("%w: %q", ErrCapabilityDenied, req.Capability)
	}

	// Temporal validity, on the verifier's own clock, with the skew margin only
	// ever shortening the honoured window (ADR-0048). not_yet_valid tolerates a
	// clock behind by up to the margin; expired refuses a margin early.
	if now.Add(SkewMargin).Before(g.IssuedAt) {
		return Grant{}, fmt.Errorf("%w: valid from %s, now %s", ErrNotYetValid, g.IssuedAt, now)
	}
	if !now.Before(g.ExpiresAt.Add(-SkewMargin)) {
		return Grant{}, fmt.Errorf("%w: expired at %s, now %s", ErrExpired, g.ExpiresAt, now)
	}
	return g, nil
}

// canonicalCaps sorts and de-duplicates a capability set for the signed form.
func canonicalCaps(caps []Capability) []string {
	seen := make(map[string]struct{}, len(caps))
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		s := string(c)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func toCaps(ss []string) []Capability {
	out := make([]Capability, len(ss))
	for i, s := range ss {
		out[i] = Capability(s)
	}
	return out
}

func hasCapability(caps []Capability, want Capability) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func decode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
