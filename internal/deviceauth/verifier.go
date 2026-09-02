package deviceauth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/voidbind-go/rp"
)

// Scheme is the HTTP Authorization scheme a device presents, alongside the
// existing "Bearer". A device has no token to bear; it presents a cert and a
// proof of possession, so it needs its own scheme rather than overloading
// Bearer with a value the bearer verifier would only reject.
const Scheme = "Device"

// credentialSeparator joins the two halves of a device credential: the
// user-signed enrolment cert, and the device's fresh possession proof. It is
// enrolment's constant rather than a second spelling of "~", so the client that
// assembles the credential and this verifier that splits it cannot drift apart.
const credentialSeparator = enrolment.CredentialSeparator

// ErrMalformedCredential is a device credential that is not "<cert>~<proof>".
var ErrMalformedCredential = errors.New("deviceauth: malformed device credential")

// Authenticated is a successfully device-authenticated caller: the principal it
// acts as, and the keys that proved it.
type Authenticated struct {
	PrincipalID   string
	PrincipalName string
	UserID        string
	UserKey       string
	DeviceKey     string
}

// Verify authenticates a presented device credential and resolves the principal
// it acts as, or returns why it does not.
//
// The chain is every check ADR-0048 requires, in order, and each refuses
// distinctly so a rejected device and an attack are told apart:
//
//  1. the cert's claimed user is only a hint — used to find the pinned key;
//  2. the user must be pinned here (ErrUnknownUser) — enrol before trust;
//  3. the cert must verify against that pinned key and be unexpired (the
//     enrolment.* refusals) — the user really did vouch for this device;
//  4. the device must be enrolled here and not revoked (ErrUnknownDevice /
//     ErrDeviceRevoked) — a leaked cert for a device the server never accepted,
//     or one since revoked, authenticates nobody;
//  5. the possession proof must be signed by the cert's device key and bound to
//     this cert (the possession refusals) — the caller HOLDS the key, it did not
//     merely copy the cert.
//
// No token is issued at any step: the identity is the device key and the
// user-signed cert, verified offline against a pinned key — the acceptance
// sentence's first half.
func (s *Store) Verify(ctx context.Context, credential string, now time.Time) (Authenticated, error) {
	certToken, proof, ok := strings.Cut(credential, credentialSeparator)
	if !ok || certToken == "" || proof == "" {
		return Authenticated{}, ErrMalformedCredential
	}

	user, auth, err := s.verifyCert(ctx, certToken, now)
	if err != nil {
		return Authenticated{}, err
	}

	device, err := s.LookupDevice(ctx, auth.DeviceKey)
	if err != nil {
		return Authenticated{}, err
	}
	if err := device.Active(now); err != nil {
		return Authenticated{}, err
	}
	// A device row is unique by key, but assert the user match anyway: it is the
	// invariant the CASCADE maintains, and asserting it here means a future
	// change that breaks it fails closed rather than authenticating as the wrong
	// user.
	if device.UserID != user.ID {
		return Authenticated{}, ErrCertMismatch
	}

	if err := verifyPossession(proof, auth.DeviceKey, certToken, now); err != nil {
		return Authenticated{}, err
	}

	return Authenticated{
		PrincipalID:   user.PrincipalID,
		PrincipalName: user.Name,
		UserID:        user.ID,
		UserKey:       user.PublicKey,
		DeviceKey:     auth.DeviceKey,
	}, nil
}

// verifyCert is steps 1–3 of Verify: the cert's claimed user is a hint to find
// the pinned key, the user must be pinned here, and the cert must verify
// against that pinned key and be unexpired. It is shared with self-enrolment
// (ADR-0067) so the cert a device enrols with is judged by exactly the rule it
// will later authenticate under — one verifier, not two that can drift.
//
// Steps 1 and 3 are exactly voidbind-go/rp's pure verifier: the shared trust
// core All Thing and heyarr hold in common (ADR-0048, generalised from this
// very method). We back it with the single key we just resolved from the
// device store, pinned under the same claimed user, so it reports the
// identical enrolment.* refusals this method always has. Step 2's
// ErrUnknownUser stays LookupUser's — the pin is present by construction here,
// so rp never reaches its own unknown-user gate.
func (s *Store) verifyCert(ctx context.Context, certToken string, now time.Time) (User, rp.Authenticated, error) {
	claimedUser, err := enrolment.CertUser(certToken)
	if err != nil {
		return User{}, rp.Authenticated{}, err
	}
	user, err := s.LookupUser(ctx, claimedUser)
	if err != nil {
		return User{}, rp.Authenticated{}, err
	}
	pinned, err := identity.ParsePublicKey(user.PublicKey)
	if err != nil {
		return User{}, rp.Authenticated{}, err
	}
	auth, err := rp.Verifier{Trust: rp.MemTrust{claimedUser: pinned}}.Verify(certToken, now)
	if err != nil {
		return User{}, rp.Authenticated{}, err
	}
	return user, auth, nil
}

// verifyPossession is step 5 of Verify: the proof must be signed by the cert's
// device key and bound to this cert — the caller HOLDS the key, it did not
// merely copy the cert. Shared with self-enrolment for the same reason
// verifyCert is.
func verifyPossession(proof, deviceKey, certToken string, now time.Time) error {
	devicePub, err := identity.ParsePublicKey(deviceKey)
	if err != nil {
		return err
	}
	return enrolment.VerifyPossession(proof, devicePub, certToken, now)
}
