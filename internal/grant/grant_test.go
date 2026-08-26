package grant

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// A fixed instant so expiry is a fact and not a wait (ADR-0017): every temporal
// assertion moves this, never the wall clock.
var testNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// issuer generates a keypair and returns its signer, rendered id, and a trust
// store that has pinned it — the ordinary "this issuer is enrolled" case.
func issuer(t *testing.T) (ed25519.PrivateKey, string, Keys) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	store := Keys{}
	id := store.Enrol(pub)
	if id != identity.FormatPublicKey(pub) {
		t.Fatalf("enrol id %q != rendered %q", id, identity.FormatPublicKey(pub))
	}
	return priv, id, store
}

// grantOf mints a read grant for (principal, resource) valid for an hour.
func grantOf(t *testing.T, priv ed25519.PrivateKey, id, principal, resource string, caps ...Capability) string {
	t.Helper()
	if len(caps) == 0 {
		caps = []Capability{CapabilityRead}
	}
	tok, err := Grant{
		Issuer:       id,
		Principal:    principal,
		Resource:     resource,
		Capabilities: caps,
		IssuedAt:     testNow,
		ExpiresAt:    testNow.Add(time.Hour),
	}.Sign(priv)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return tok
}

func TestRoundTripHonoursAMatchingRequest(t *testing.T) {
	t.Parallel()
	priv, id, store := issuer(t)
	tok := grantOf(t, priv, id, "user-a", "asset-1")

	g, err := Verify(tok, store, Request{Principal: "user-a", Resource: "asset-1", Capability: CapabilityRead}, testNow)
	if err != nil {
		t.Fatalf("a matching request was refused: %v (%s)", err, ReasonFor(err))
	}
	if g.Principal != "user-a" || g.Resource != "asset-1" || g.Issuer != id {
		t.Fatalf("verified grant does not round-trip: %+v", g)
	}
	if !hasCapability(g.Capabilities, CapabilityRead) {
		t.Fatalf("verified grant lost its capability: %+v", g.Capabilities)
	}
}

// Each of the four bindings is proven to constrain by a single-fault grant that
// differs from the request in exactly one field, asserted on the NAMED reason —
// the reasons share substrings, so this is assert-equals territory.
func TestEachBindingConstrains(t *testing.T) {
	t.Parallel()
	priv, id, store := issuer(t)
	// A grant for user-a/asset-1/read only.
	tok := grantOf(t, priv, id, "user-a", "asset-1")

	tests := []struct {
		name string
		req  Request
		want Reason
		err  error
	}{
		{"matching", Request{"user-a", "asset-1", CapabilityRead}, ReasonOK, nil},
		{"wrong principal", Request{"user-b", "asset-1", CapabilityRead}, ReasonPrincipalMismatch, ErrPrincipalMismatch},
		{"wrong resource", Request{"user-a", "asset-2", CapabilityRead}, ReasonResourceMismatch, ErrResourceMismatch},
		{"read grant, write asked", Request{"user-a", "asset-1", CapabilityWrite}, ReasonCapabilityDenied, ErrCapabilityDenied},
		// Prefix-adjacent cases: the match must be EXACT, not a prefix, in
		// either direction. Mutation testing surfaced that a HasPrefix
		// weakening of either check survived the plain wrong-value cases above
		// — these kill it. (grant is user-a / asset-1.)
		{"principal is a prefix of the grant's", Request{"user", "asset-1", CapabilityRead}, ReasonPrincipalMismatch, ErrPrincipalMismatch},
		{"principal extends the grant's", Request{"user-aa", "asset-1", CapabilityRead}, ReasonPrincipalMismatch, ErrPrincipalMismatch},
		{"resource is a prefix of the grant's", Request{"user-a", "asset", CapabilityRead}, ReasonResourceMismatch, ErrResourceMismatch},
		{"resource extends the grant's", Request{"user-a", "asset-11", CapabilityRead}, ReasonResourceMismatch, ErrResourceMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(tok, store, tc.req, testNow)
			if tc.err == nil {
				if err != nil {
					t.Fatalf("want honoured, got %v", err)
				}
				return
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("want %v, got %v", tc.err, err)
			}
			if got := ReasonFor(err); got != tc.want {
				t.Fatalf("reason: want %q, got %q", tc.want, got)
			}
		})
	}
}

// A grant naming an issuer the store has not pinned is refused before any field
// is trusted — the enrol-before-trust gate (ADR-0032, ADR-0048). A key issued
// and immediately honoured is unspellable because an empty store honours nothing.
func TestUnknownIssuerIsRefused(t *testing.T) {
	t.Parallel()
	priv, id, _ := issuer(t)
	tok := grantOf(t, priv, id, "user-a", "asset-1")

	_, err := Verify(tok, Keys{}, Request{"user-a", "asset-1", CapabilityRead}, testNow)
	if !errors.Is(err, ErrUnknownIssuer) {
		t.Fatalf("want ErrUnknownIssuer, got %v", err)
	}
	if r := ReasonFor(err); r != ReasonUnknownIssuer {
		t.Fatalf("reason: want %q, got %q", ReasonUnknownIssuer, r)
	}
}

// A grant signed by one key but pinned under another enrolled issuer's id fails
// the signature: the issuer field only SELECTS the key; the signature is the
// authority. This is the "second door" #285 asks to be shown stays shut.
func TestSignatureCannotBeRedirectedToAnotherEnrolledIssuer(t *testing.T) {
	t.Parallel()
	privA, idA, store := issuer(t)
	// A second enrolled issuer, whose key is also pinned.
	pubB, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	idB := store.Enrol(pubB)

	// A grant that CLAIMS issuer B but is signed by A's key. Sign refuses the
	// honest path (issuer must match the key), so forge the payload by hand.
	tok := grantOf(t, privA, idA, "user-a", "asset-1")
	forged := repointIssuer(t, tok, idB)

	_, verr := Verify(forged, store, Request{"user-a", "asset-1", CapabilityRead}, testNow)
	if !errors.Is(verr, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature for a grant redirected to issuer B, got %v", verr)
	}
}

// A single flipped byte of the signed payload fails verification.
func TestTamperedSignatureIsRefused(t *testing.T) {
	t.Parallel()
	priv, id, store := issuer(t)
	tok := grantOf(t, priv, id, "user-a", "asset-1")

	body, sigEnc, _ := strings.Cut(tok, ".")
	sig, err := decode(sigEnc)
	if err != nil {
		t.Fatal(err)
	}
	sig[len(sig)/2] ^= 0x01
	tampered := body + "." + encode(sig)

	if _, err := Verify(tampered, store, Request{"user-a", "asset-1", CapabilityRead}, testNow); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a tampered signature verified or gave the wrong reason: %v", err)
	}
}

// Tampering any signed field is caught: the grant is refused, never honoured,
// whichever field the flip lands in (a mutated issuer selects a different key or
// none; any other field breaks the signature).
func TestTamperedPayloadIsNeverHonoured(t *testing.T) {
	t.Parallel()
	priv, id, store := issuer(t)
	tok := grantOf(t, priv, id, "user-a", "asset-1")

	body, sig, _ := strings.Cut(tok, ".")
	raw, err := decode(body)
	if err != nil {
		t.Fatal(err)
	}
	// Flip each byte in turn; none may yield a honoured grant.
	for i := range raw {
		mutated := append([]byte(nil), raw...)
		mutated[i] ^= 0x01
		tampered := encode(mutated) + "." + sig
		if _, err := Verify(tampered, store, Request{"user-a", "asset-1", CapabilityRead}, testNow); err == nil {
			t.Fatalf("byte %d: a tampered payload was honoured", i)
		}
	}
}

func TestMalformedTokens(t *testing.T) {
	t.Parallel()
	_, _, store := issuer(t)
	for _, tok := range []string{"", "no-dot", "not-base64!.also-not", "."} {
		if _, err := Verify(tok, store, Request{"user-a", "asset-1", CapabilityRead}, testNow); !errors.Is(err, ErrMalformed) {
			t.Fatalf("token %q: want ErrMalformed, got %v", tok, err)
		}
	}
}

// Expiry is refused by the verifier's own clock, with the issuer nowhere in the
// call — proven by advancing the injected clock past the grant's life, never by
// sleeping.
func TestExpiryIsRefusedByTheLocalClock(t *testing.T) {
	t.Parallel()
	priv, id, store := issuer(t)
	tok := grantOf(t, priv, id, "user-a", "asset-1") // lives testNow .. +1h

	// Well past expiry.
	_, err := Verify(tok, store, Request{"user-a", "asset-1", CapabilityRead}, testNow.Add(2*time.Hour))
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	if r := ReasonFor(err); r != ReasonExpired {
		t.Fatalf("reason: want %q, got %q", ReasonExpired, r)
	}
}

// Skew fails toward refusal: a verifier whose clock reads a margin BEFORE the
// true expiry already refuses (the window is shortened, never extended), and one
// reading before issue refuses not_yet_valid. The property is that the honoured
// window is a strict subset of [issued, expiry].
func TestClockSkewFailsTowardRefusal(t *testing.T) {
	t.Parallel()
	priv, id, store := issuer(t)
	tok := grantOf(t, priv, id, "user-a", "asset-1") // [testNow, testNow+1h]
	req := Request{"user-a", "asset-1", CapabilityRead}

	// A verifier reading exactly the true expiry: already refused, because the
	// margin is subtracted from the honoured window (safe).
	if _, err := Verify(tok, store, req, testNow.Add(time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("at true expiry the grant should already be refused, got %v", err)
	}
	// A margin before true expiry: the first instant the shortened window refuses.
	if _, err := Verify(tok, store, req, testNow.Add(time.Hour-SkewMargin)); !errors.Is(err, ErrExpired) {
		t.Fatalf("a margin before expiry should refuse (shortened window), got %v", err)
	}
	// Comfortably inside the shortened window: honoured.
	if _, err := Verify(tok, store, req, testNow.Add(time.Hour-SkewMargin-time.Minute)); err != nil {
		t.Fatalf("inside the shortened window the grant must be honoured, got %v", err)
	}
	// Before issue, beyond the margin: not yet valid (safe on the other side).
	if _, err := Verify(tok, store, req, testNow.Add(-2*SkewMargin)); !errors.Is(err, ErrNotYetValid) {
		t.Fatalf("before issue beyond the margin should be not_yet_valid, got %v", err)
	}
}

// A grant that carries write authorises write, and one that carries both read
// and write authorises each — capabilities are a set, not a bearer bit.
func TestCapabilitySetIsHonouredMemberByMember(t *testing.T) {
	t.Parallel()
	priv, id, store := issuer(t)
	tok := grantOf(t, priv, id, "user-a", "asset-1", CapabilityRead, CapabilityWrite)

	for _, c := range []Capability{CapabilityRead, CapabilityWrite} {
		if _, err := Verify(tok, store, Request{"user-a", "asset-1", c}, testNow); err != nil {
			t.Fatalf("capability %q should be honoured, got %v", c, err)
		}
	}
	if _, err := Verify(tok, store, Request{"user-a", "asset-1", Capability("delete")}, testNow); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("an ungranted capability should be denied, got %v", err)
	}
}

// Sign refuses to mint a grant that could not be honoured: the TTL cap is the
// revocation window and cannot be widened by minting.
func TestSignEnforcesItsInvariants(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(nil)
	id := identity.FormatPublicKey(pub)
	base := Grant{Issuer: id, Principal: "user-a", Resource: "asset-1", Capabilities: []Capability{CapabilityRead}, IssuedAt: testNow, ExpiresAt: testNow.Add(time.Hour)}

	tests := []struct {
		name string
		mut  func(g *Grant)
		want error
	}{
		{"a grant past MaxTTL", func(g *Grant) { g.ExpiresAt = g.IssuedAt.Add(MaxTTL + time.Minute) }, ErrTTLTooLong},
		{"no principal", func(g *Grant) { g.Principal = "" }, ErrIncomplete},
		{"no resource", func(g *Grant) { g.Resource = "" }, ErrIncomplete},
		{"no capability", func(g *Grant) { g.Capabilities = nil }, ErrIncomplete},
		{"expiry before issue", func(g *Grant) { g.ExpiresAt = g.IssuedAt.Add(-time.Minute) }, ErrIncomplete},
		{"issuer is not the signing key", func(g *Grant) { g.Issuer = "ed25519:" + strings.Repeat("00", 32) }, ErrIssuerMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := base
			tc.mut(&g)
			if _, err := g.Sign(priv); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}

	// Exactly MaxTTL is allowed — the boundary is inclusive.
	g := base
	g.ExpiresAt = g.IssuedAt.Add(MaxTTL)
	if _, err := g.Sign(priv); err != nil {
		t.Fatalf("a grant of exactly MaxTTL should sign, got %v", err)
	}
}

// repointIssuer rewrites the issuer field of a signed token WITHOUT re-signing —
// a forgery helper, used to prove the signature, not the issuer hint, is the
// authority. It leaves the original signature in place.
func repointIssuer(t *testing.T, token, newIssuer string) string {
	t.Helper()
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("token has no signature: %q", token)
	}
	raw, err := decode(body)
	if err != nil {
		t.Fatal(err)
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	p.Issuer = newIssuer
	reraw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return encode(reraw) + "." + sig
}
