package deviceauth_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
)

// enrolActor pins a user (auto-named, so two distinct users never collide on the
// UNIQUE principals.name) and enrols its device.
func enrolActor(t *testing.T, f *fixture, a *actor) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.store.EnrolUser(ctx, a.userKey, ""); err != nil {
		t.Fatalf("enrol user: %v", err)
	}
	if _, err := f.store.EnrolDevice(ctx, a.cert, "phone"); err != nil {
		t.Fatalf("enrol device: %v", err)
	}
}

// Adversarial synthetic tests for the device-authentication chain: credential
// shapes and cross-actor confusions the hand-written suite does not cover. They
// reuse the fixture and actor helpers from deviceauth_test.go (same package).
// The threat model is an attacker who holds SOME valid material — a cert they
// captured, a device key that is not the one a cert names, a possession proof
// for a different presentation — and tries to assemble it into an accepted
// credential.

// A credential is "<cert>~<possession>". Every degenerate shape is refused, and
// none panics.
func TestMalformedCredentialShapes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := newActor(t)
	f.enrol(t, a)
	ctx := context.Background()
	proof := strings.SplitN(a.credential(t, now), "~", 2)[1]

	cases := []string{
		"",                              // empty
		a.cert,                          // cert only, no separator
		"~" + proof,                     // empty cert half
		a.cert + "~",                    // empty possession half
		"~",                             // both empty
		a.cert + "~" + proof + "~extra", // an extra separator: the possession half is now junk
		"not-a-cert~not-a-proof",
	}
	for i, cred := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("credential %d panicked: %v", i, r)
				}
			}()
			if _, err := f.store.Verify(ctx, cred, now); err == nil {
				t.Fatalf("credential %d was accepted: %q", i, cred)
			}
		}()
	}
}

// A valid cert presented with a possession proof signed by a DIFFERENT device
// key — the key an attacker actually holds — is refused: the possession must be
// by the key the cert names, which is the whole point of possession.
func TestPossessionMustBeByTheCertsDeviceKey(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := newActor(t)
	f.enrol(t, a)
	ctx := context.Background()

	_, attackerKey, _ := ed25519.GenerateKey(nil)
	badProof, err := enrolment.SignPossession(attackerKey, a.cert, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Verify(ctx, a.cert+"~"+badProof, now); !errors.Is(err, enrolment.ErrPossessionSignature) {
		t.Fatalf("possession by the wrong key must fail, got %v", err)
	}
}

// One user's device cannot borrow another user's cert. B's cert names B's device
// key; an attacker holding A's device key cannot produce a possession proof for
// B's device key, so presenting B's cert with any proof A can make is refused.
func TestOneUsersDeviceCannotBorrowAnothersCert(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	alice := newActor(t)
	bob := newActor(t)
	// Enrol both directly with auto-generated names (empty → "user-<suffix>"),
	// since the shared enrol helper hardcodes one name and principals.name is
	// UNIQUE — two distinct users cannot share a name.
	enrolActor(t, f, alice)
	enrolActor(t, f, bob)

	// Alice tries to authenticate as Bob: she presents Bob's cert, but can only
	// sign a possession proof with her OWN device key.
	proof, err := enrolment.SignPossession(alice.devicePriv, bob.cert, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Verify(ctx, bob.cert+"~"+proof, now); !errors.Is(err, enrolment.ErrPossessionSignature) {
		t.Fatalf("borrowing another user's cert must fail possession, got %v", err)
	}
}

// A possession proof minted for one cert cannot be replayed with another cert,
// even both belonging to the same device across a re-enrolment.
func TestPossessionCannotBeReplayedAcrossCerts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := newActor(t)
	f.enrol(t, a)
	ctx := context.Background()

	// A proof bound to a different cert string.
	proof, err := enrolment.SignPossession(a.devicePriv, "some-other-cert", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Verify(ctx, a.cert+"~"+proof, now); !errors.Is(err, enrolment.ErrPossessionCert) {
		t.Fatalf("a proof bound to another cert must be refused, got %v", err)
	}
}

// After a user is revoked (cascade), a previously-valid credential authenticates
// nobody — refused unknown_user, on the very next call.
func TestCredentialDiesWithTheUser(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := newActor(t)
	f.enrol(t, a)
	ctx := context.Background()

	if _, err := f.store.Verify(ctx, a.credential(t, now), now); err != nil {
		t.Fatalf("precondition: valid credential works, got %v", err)
	}
	if _, err := f.store.RevokeUser(ctx, a.userKey); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Verify(ctx, a.credential(t, now), now); !errors.Is(err, deviceauth.ErrUnknownUser) {
		t.Fatalf("a revoked user's credential must be refused unknown_user, got %v", err)
	}
}

// Possession is freshly bounded: a credential minted well in the past is refused
// once its short possession window has passed, even though the long-lived cert
// is still valid — the two lifetimes are independent, and the short one governs
// liveness.
func TestStalePossessionIsRefusedWhileTheCertStillLives(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := newActor(t)
	f.enrol(t, a)
	ctx := context.Background()

	cred := a.credential(t, now) // possession minted at now
	later := now.Add(enrolment.PossessionTTL + time.Minute)
	// The cert (90 days) is still valid at `later`, but the possession is stale.
	if _, err := f.store.Verify(ctx, cred, later); !errors.Is(err, enrolment.ErrPossessionExpired) {
		t.Fatalf("a stale possession must be refused even under a live cert, got %v", err)
	}
}
