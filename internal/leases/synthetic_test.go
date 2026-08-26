package leases_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/grant"
)

// Adversarial synthetic tests for the lease store: malformed tokens, cross-type
// confusion, and a sibling-key provider that fails — cases the hand-written and
// cross-site suites do not cover. They reuse newStore / peerStore / fakeSiblings
// from the sibling test files (same package).

// A garbage or cross-type token is refused, never a panic — a lease token that
// is actually something else (an enrolment credential, random bytes) does not
// slip through Honour.
func TestHonourRefusesGarbageAndCrossTypeTokens(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	ctx := context.Background()
	req := grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}

	tokens := []string{
		"",
		"not-a-token",
		"a.b.c",
		"cert-half~possession-half", // a device-auth credential shape, not a grant
		"eyJ2IjoxfQ.AAAA",           // valid base64 {"v":1} + junk sig
	}
	for i, tok := range tokens {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("token %d panicked: %v", i, r)
				}
			}()
			if _, err := store.Honour(ctx, tok, req, now); err == nil {
				t.Fatalf("token %d was honoured: %q", i, tok)
			}
		}()
	}
}

// erroringSiblings fails when asked for the trust set.
type erroringSiblings struct{}

func (erroringSiblings) PeerKeys(context.Context) (map[string]ed25519.PublicKey, error) {
	return nil, errors.New("membership is unreachable")
}

// If the sibling-key provider errors, Honour must FAIL, not fall back to
// trusting self alone (which would silently honour this peer's own leases while
// pretending it checked the sibling set). Fail closed, loudly.
func TestHonourFailsWhenTheSiblingProviderErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, keyA, _ := ed25519.GenerateKey(nil)
	// Build a store whose Siblings provider errors, but which can still issue
	// (so we have a self-signed token that WOULD verify against self).
	store := peerStore(t, keyA, erroringSiblings{})
	lease, err := store.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}
	if _, err := store.Honour(ctx, lease.Token, req, now); err == nil {
		t.Fatal("Honour must fail when the sibling provider errors, not fall back to self-only trust")
	}
}

// A lease whose issuer id is in the sibling set but whose signature is by a
// DIFFERENT key (a poisoned pin) is refused — the pin selects, the signature
// decides.
func TestPoisonedSiblingPinDoesNotVerifyAForeignSignature(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A real sibling A issues a lease.
	_, keyA, _ := ed25519.GenerateKey(nil)
	issuer := peerStore(t, keyA, nil)
	lease, err := issuer.Issue(ctx, "user-a", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	idA := grant.Keys{}.Enrol(keyA.Public().(ed25519.PublicKey))

	// B pins A's id, but to a STRANGER's key (a poisoned membership row).
	strangerPub, _, _ := ed25519.GenerateKey(nil)
	_, keyB, _ := ed25519.GenerateKey(nil)
	b := peerStore(t, keyB, fakeSiblings{idA: strangerPub})

	req := grant.Request{Principal: "user-a", Resource: "asset-1", Capability: grant.CapabilityRead}
	if _, err := b.Honour(ctx, lease.Token, req, now); !errors.Is(err, grant.ErrBadSignature) {
		t.Fatalf("a poisoned pin must not verify A's real signature, got %v", err)
	}
}
