package deviceauth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
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

	claimedUser, err := enrolment.CertUser(certToken)
	if err != nil {
		return Authenticated{}, err
	}
	user, err := s.LookupUser(ctx, claimedUser)
	if err != nil {
		return Authenticated{}, err
	}
	pinned, err := identity.ParsePublicKey(user.PublicKey)
	if err != nil {
		return Authenticated{}, err
	}
	cert, err := enrolment.VerifyCert(certToken, pinned, now)
	if err != nil {
		return Authenticated{}, err
	}

	device, err := s.LookupDevice(ctx, cert.Device)
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

	devicePub, err := identity.ParsePublicKey(cert.Device)
	if err != nil {
		return Authenticated{}, err
	}
	if err := enrolment.VerifyPossession(proof, devicePub, certToken, now); err != nil {
		return Authenticated{}, err
	}

	return Authenticated{
		PrincipalID:   user.PrincipalID,
		PrincipalName: user.Name,
		UserID:        user.ID,
		UserKey:       user.PublicKey,
		DeviceKey:     cert.Device,
	}, nil
}
