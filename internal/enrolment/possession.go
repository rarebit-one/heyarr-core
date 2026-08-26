package enrolment

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A cert says a user vouches for a device key; it does not prove the caller
// HOLDS that key. Anyone who saw a cert could otherwise replay it and be its
// device. A possession proof closes that: the caller signs, with the device
// private key, a short-lived assertion bound to the very cert it is presenting.
//
// This is the same signed-and-expiring shape as a grant, turned to a different
// job. It is stateless — there is no server nonce and so no round trip and no
// state to keep — and the price of that is a replay WINDOW: a proof captured off
// a compromised channel is reusable until it expires. That window is
// PossessionTTL, deliberately tiny, and over TLS the proof is not exposed in
// transit at all. A server-issued challenge would shrink the window to zero at
// the cost of a round trip and per-request state; it is the hardening path if
// the threat model ever needs it (recorded here, not built).

// PossessionTTL is the short life of a possession proof. It is the replay window
// above, so it is minutes, not hours — a caller re-proves possession cheaply on
// each new authenticated session, and a stolen proof is stale almost at once.
const PossessionTTL = 2 * time.Minute

// PossessionSkew is the clock tolerance for a possession proof, and it is
// SMALL — deliberately far smaller than grant.SkewMargin, and applied
// differently. A grant lives for hours, so shortening its window by a few
// minutes is free; a possession proof lives for PossessionTTL, so subtracting a
// grant-sized margin would shorten it to nothing (the honoured window would be
// negative). Instead expiry is strict — a proof is honoured only while its own
// clock says it is unexpired, no grace, which keeps the "never honour an expired
// proof" direction — and this tolerance is added only to the not-yet-valid side,
// so a device whose clock runs a little ahead of the server is not refused. A
// server clock running behind over-honours by at most the real skew, which on
// NTP-synced hosts is seconds against a two-minute window; if the skew ever
// exceeds the TTL, valid proofs are refused (safe, and a sign NTP is down).
const PossessionSkew = 30 * time.Second

// The possession refusals, distinct because they mean different things: an
// expiry is the proof simply being stale, a bad signature is someone who does
// not hold the device key, and a cert mismatch is a proof made for a different
// presentation.
var (
	ErrPossessionMalformed = errors.New("enrolment: malformed possession proof")
	ErrPossessionSignature = errors.New("enrolment: possession proof is not signed by the device key")
	ErrPossessionExpired   = errors.New("enrolment: possession proof has expired")
	ErrPossessionNotYet    = errors.New("enrolment: possession proof is not yet valid")
	ErrPossessionCert      = errors.New("enrolment: possession proof is bound to a different cert")
)

type possessionPayload struct {
	V        int    `json:"v"`
	CertHash string `json:"crt"` // sha256 of the cert token this proof accompanies
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

// SignPossession proves the holder of devicePriv is presenting certToken, valid
// for ttl from now (a zero ttl uses PossessionTTL). The proof is bound to the
// cert by its hash, so a proof cannot be lifted onto a different presentation.
func SignPossession(devicePriv ed25519.PrivateKey, certToken string, now time.Time, ttl time.Duration) (string, error) {
	if len(devicePriv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("enrolment: a device signing key is required")
	}
	if certToken == "" {
		return "", fmt.Errorf("%w: no cert to bind to", ErrPossessionMalformed)
	}
	if now.IsZero() {
		return "", fmt.Errorf("enrolment: a clock is required")
	}
	if ttl <= 0 {
		ttl = PossessionTTL
	}
	body, err := json.Marshal(possessionPayload{
		V:        Version,
		CertHash: certHash(certToken),
		IssuedAt: now.Unix(),
		Expires:  now.Add(ttl).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("enrolment: encoding possession: %w", err)
	}
	return encode(body) + "." + encode(ed25519.Sign(devicePriv, body)), nil
}

// VerifyPossession checks a proof was made by deviceKey, for certToken, and is
// unexpired on the verifier's own clock.
//
// deviceKey comes from a cert that VerifyCert already authenticated, so this
// step adds the one thing the cert cannot: that the caller holds the private
// half. Order matches the rest of the package — signature before fields,
// temporal validity last, and the skew margin only shortens the window.
func VerifyPossession(token string, deviceKey ed25519.PublicKey, certToken string, now time.Time) error {
	if len(deviceKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: no device key to check against", ErrPossessionSignature)
	}
	bodyEnc, sigEnc, ok := strings.Cut(token, ".")
	if !ok {
		return ErrPossessionMalformed
	}
	body, err := decode(bodyEnc)
	if err != nil {
		return ErrPossessionMalformed
	}
	sig, err := decode(sigEnc)
	if err != nil {
		return ErrPossessionMalformed
	}
	if !ed25519.Verify(deviceKey, body, sig) {
		return ErrPossessionSignature
	}
	var p possessionPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return ErrPossessionMalformed
	}
	if p.V != Version || p.CertHash == "" {
		return ErrPossessionMalformed
	}
	if p.CertHash != certHash(certToken) {
		return ErrPossessionCert
	}
	issued := time.Unix(p.IssuedAt, 0).UTC()
	expires := time.Unix(p.Expires, 0).UTC()
	// not-yet-valid tolerates a device clock a little ahead of the server;
	// expiry is strict, with no margin, so an expired proof is never honoured
	// (see PossessionSkew for why the two sides are asymmetric).
	if now.Add(PossessionSkew).Before(issued) {
		return fmt.Errorf("%w: valid from %s, now %s", ErrPossessionNotYet, issued, now)
	}
	if !now.Before(expires) {
		return fmt.Errorf("%w: expired at %s, now %s", ErrPossessionExpired, expires, now)
	}
	return nil
}

func certHash(certToken string) string {
	sum := sha256.Sum256([]byte(certToken))
	return encode(sum[:])
}
