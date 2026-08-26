package grant_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/grant"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// Adversarial synthetic tests for the grant package: cases the hand-written
// suite in grant_test.go does not exercise — hand-forged payloads a legitimate
// Sign would never produce, boundary instants, capability confusion, and
// malformed tokens that must be refused rather than panic. The threat model is
// an attacker who can craft any bytes AND get the issuer to sign them (the
// strongest position short of holding the key), so every "forged" token here is
// signed by the real issuer key over attacker-chosen JSON.

var synthNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// synthIssuer returns a signer, its rendered id, and a trust store holding it.
func synthIssuer(t *testing.T) (ed25519.PrivateKey, string, grant.Keys) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	ks := grant.Keys{}
	return priv, ks.Enrol(pub), ks
}

// forge signs attacker-chosen payload fields with the issuer's real key. This is
// the sharp tool: it produces a token whose signature verifies but whose fields
// a legitimate Sign would have refused, so it probes whether Verify re-checks
// what Sign guarantees.
func forge(t *testing.T, priv ed25519.PrivateKey, fields map[string]any) string {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, body)
	enc := base64.RawURLEncoding.EncodeToString
	return enc(body) + "." + enc(sig)
}

func basePayload(issuer string) map[string]any {
	return map[string]any{
		"v": 1, "iss": issuer, "prn": "user-a", "res": "asset-1",
		"cap": []string{"read"}, "iat": synthNow.Unix(), "exp": synthNow.Add(time.Hour).Unix(),
	}
}

func readReq() grant.Request {
	return grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}
}

// A signed grant with an EMPTY capability set — which Sign refuses (ErrIncomplete)
// but an attacker could forge — must not authorise anything.
func TestForgedEmptyCapabilitySetAuthorisesNothing(t *testing.T) {
	t.Parallel()
	priv, id, ks := synthIssuer(t)
	p := basePayload(id)
	p["cap"] = []string{}
	tok := forge(t, priv, p)
	if _, err := grant.Verify(tok, ks, readReq(), synthNow); !errors.Is(err, grant.ErrCapabilityDenied) {
		t.Fatalf("an empty capability set must deny, got %v", err)
	}
}

// Capability matching must be exact, not case-folded or prefixed: "Read", "READ"
// and "rea" are not "read".
func TestCapabilityMatchIsExactAndCaseSensitive(t *testing.T) {
	t.Parallel()
	priv, id, ks := synthIssuer(t)
	for _, granted := range []string{"Read", "READ", "rea", "read "} {
		p := basePayload(id)
		p["cap"] = []string{granted}
		tok := forge(t, priv, p)
		if _, err := grant.Verify(tok, ks, readReq(), synthNow); !errors.Is(err, grant.ErrCapabilityDenied) {
			t.Fatalf("capability %q must not satisfy a request for %q, got %v", granted, "read", err)
		}
	}
}

// A resource or principal that differs only by trailing/leading whitespace or a
// unicode confusable is a different resource — exact bytes, no normalisation.
func TestPrincipalAndResourceAreExactBytes(t *testing.T) {
	t.Parallel()
	priv, id, ks := synthIssuer(t)

	p := basePayload(id)
	p["res"] = "asset-1 " // trailing space
	if _, err := grant.Verify(forge(t, priv, p), ks, readReq(), synthNow); !errors.Is(err, grant.ErrResourceMismatch) {
		t.Fatalf("a trailing space makes a different resource, got %v", err)
	}

	p = basePayload(id)
	p["prn"] = "usеr-a" // the 'е' is Cyrillic U+0435, a confusable
	if _, err := grant.Verify(forge(t, priv, p), ks, readReq(), synthNow); !errors.Is(err, grant.ErrPrincipalMismatch) {
		t.Fatalf("a unicode-confusable principal must not match, got %v", err)
	}
}

// The trust store maps an issuer id to a key. If it maps the id to a DIFFERENT
// key than the one that signed, verification fails — a substituted pin cannot
// rubber-stamp a foreign signature.
func TestKeySubstitutionInTheTrustStoreFails(t *testing.T) {
	t.Parallel()
	priv, id, _ := synthIssuer(t)
	tok := forge(t, priv, basePayload(id))

	// A store that claims `id` but holds a stranger's key.
	otherPub, _, _ := ed25519.GenerateKey(nil)
	poisoned := grant.Keys{id: otherPub}
	if _, err := grant.Verify(tok, poisoned, readReq(), synthNow); !errors.Is(err, grant.ErrBadSignature) {
		t.Fatalf("a substituted pin must not verify a foreign signature, got %v", err)
	}
}

// A signature lifted from a DIFFERENT valid grant does not verify against this
// payload (no signature reuse).
func TestSignatureFromAnotherGrantDoesNotTransfer(t *testing.T) {
	t.Parallel()
	priv, id, ks := synthIssuer(t)
	a := forge(t, priv, basePayload(id))
	other := basePayload(id)
	other["res"] = "asset-2"
	b := forge(t, priv, other)

	bodyA := strings.SplitN(a, ".", 2)[0]
	sigB := strings.SplitN(b, ".", 2)[1]
	frankenstein := bodyA + "." + sigB
	if _, err := grant.Verify(frankenstein, ks, readReq(), synthNow); !errors.Is(err, grant.ErrBadSignature) {
		t.Fatalf("a signature from another grant must not transfer, got %v", err)
	}
}

// A future version is refused as malformed, not verified against the v1 reading
// of its fields.
func TestFutureVersionIsRefused(t *testing.T) {
	t.Parallel()
	priv, id, ks := synthIssuer(t)
	p := basePayload(id)
	p["v"] = 2
	if _, err := grant.Verify(forge(t, priv, p), ks, readReq(), synthNow); !errors.Is(err, grant.ErrMalformed) {
		t.Fatalf("a v2 token must be malformed to a v1 verifier, got %v", err)
	}
}

// Garbage, truncated and structurally broken tokens are refused, never a panic.
func TestMalformedTokensAreRefusedNotPanicked(t *testing.T) {
	t.Parallel()
	_, _, ks := synthIssuer(t)
	longB64 := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 100_000)))
	tokens := []string{
		"", ".", "..", "a.b.c",
		"!!!.###",
		longB64 + ".", // valid base64 body, empty sig
		"." + longB64, // empty body, valid base64 sig
		base64.RawURLEncoding.EncodeToString([]byte("{not json")) + ".AAAA",
		base64.RawURLEncoding.EncodeToString([]byte(`{"v":1}`)) + ".AAAA", // missing fields
		strings.Repeat("A", 1_000_000),                                    // no dot, huge
	}
	for i, tok := range tokens {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("token %d panicked: %v", i, r)
				}
			}()
			if _, err := grant.Verify(tok, ks, readReq(), synthNow); err == nil {
				t.Fatalf("token %d was honoured: %q", i, tok)
			}
		}()
	}
}

// The effective-expiry boundary is refused, and one nanosecond inside the
// shortened window is honoured — the exact instant the skew margin defines.
func TestExpiryBoundaryIsRefusedAtTheInstant(t *testing.T) {
	t.Parallel()
	priv, id, ks := synthIssuer(t)
	// exp is a second-granularity unix value; effective expiry is exp - margin.
	exp := synthNow.Add(time.Hour)
	p := basePayload(id)
	p["exp"] = exp.Unix()
	tok := forge(t, priv, p)

	effective := exp.Add(-grant.SkewMargin)
	if _, err := grant.Verify(tok, ks, readReq(), effective); !errors.Is(err, grant.ErrExpired) {
		t.Fatalf("at the effective-expiry instant the grant must be refused, got %v", err)
	}
	if _, err := grant.Verify(tok, ks, readReq(), effective.Add(-time.Second)); err != nil {
		t.Fatalf("one second inside the window the grant must be honoured, got %v", err)
	}
}

// A grant issued far in the future (beyond the skew margin) is not yet valid —
// an attacker cannot pre-date authority into a window that has not opened.
func TestFutureDatedGrantIsNotYetValid(t *testing.T) {
	t.Parallel()
	priv, id, ks := synthIssuer(t)
	p := basePayload(id)
	future := synthNow.Add(time.Hour)
	p["iat"] = future.Unix()
	p["exp"] = future.Add(time.Hour).Unix()
	if _, err := grant.Verify(forge(t, priv, p), ks, readReq(), synthNow); !errors.Is(err, grant.ErrNotYetValid) {
		t.Fatalf("a future-dated grant must be not_yet_valid, got %v", err)
	}
}

// An empty request principal or resource matches nothing, even against a grant
// that happens to carry an empty field — a request must name what it wants.
func TestEmptyRequestFieldsMatchNothing(t *testing.T) {
	t.Parallel()
	priv, id, ks := synthIssuer(t)
	tok := forge(t, priv, basePayload(id))
	if _, err := grant.Verify(tok, ks, grant.Request{Principal: "", Resource: "asset-1", Capability: grant.CapabilityRead}, synthNow); !errors.Is(err, grant.ErrPrincipalMismatch) {
		t.Fatalf("an empty request principal must not match, got %v", err)
	}
	if _, err := grant.Verify(tok, ks, grant.Request{Principal: "user-a", Resource: "", Capability: grant.CapabilityRead}, synthNow); !errors.Is(err, grant.ErrResourceMismatch) {
		t.Fatalf("an empty request resource must not match, got %v", err)
	}
}

// Extra unknown JSON fields are ignored (forward compatibility) and do not
// change the decision — the signed fields still govern.
func TestUnknownFieldsAreIgnored(t *testing.T) {
	t.Parallel()
	priv, id, ks := synthIssuer(t)
	p := basePayload(id)
	p["evil"] = "ignore-me"
	p["admin"] = true
	if _, err := grant.Verify(forge(t, priv, p), ks, readReq(), synthNow); err != nil {
		t.Fatalf("unknown fields must be ignored, not fatal, got %v", err)
	}
}

// The issuer id in a token is only a selector: a well-formed key that is simply
// not enrolled is unknown_issuer, distinct from a signature failure.
func TestUnenrolledIssuerIsDistinctFromBadSignature(t *testing.T) {
	t.Parallel()
	priv, _, _ := synthIssuer(t)
	// Sign under a real key, but name a DIFFERENT (valid, un-enrolled) issuer.
	strangerPub, _, _ := ed25519.GenerateKey(nil)
	strangerID := identity.FormatPublicKey(strangerPub)
	p := basePayload(strangerID)
	tok := forge(t, priv, p) // signed by priv, claims strangerID
	if _, err := grant.Verify(tok, grant.Keys{}, readReq(), synthNow); !errors.Is(err, grant.ErrUnknownIssuer) {
		t.Fatalf("an un-enrolled issuer must be unknown_issuer, got %v", err)
	}
}
