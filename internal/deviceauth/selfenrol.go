package deviceauth

import (
	"context"
	"errors"
	"time"
)

// SelfEnrol enrols a device on the strength of its own credential (ADR-0067):
// the user-signed cert and a fresh possession proof, with no admin in the loop.
//
// It is admissible because the two facts the admin route relies on are both
// present in the request and both verifiable offline: the cert proves the
// PINNED user vouched for this device key (ADR-0032's enrol-before-trust gate is
// the user pin, and it holds — an unpinned user's cert is refused), and the
// proof proves the caller holds that key. The checks are the ones Verify makes
// on every authenticated request, in the same order and through the same code,
// minus the "device is enrolled" step this call exists to satisfy.
//
// It is idempotent on the device key. A phone that lost the response and
// re-submits gets its existing row back (created=false) rather than a conflict.
// A REVOKED device is not re-enrolled — revocation is the admin's word against
// the cert's, and it wins — and a cert naming a different user than the row
// holds is refused: neither is a path back in. What is granted is nothing
// beyond enrolment: the device authenticates at the read floor, and write
// stays an admin authorisation (ADR-0065).
func (s *Store) SelfEnrol(ctx context.Context, certToken, proof, name string) (SelfEnrolment, error) {
	if certToken == "" || proof == "" {
		return SelfEnrolment{}, ErrMalformedCredential
	}
	now := s.clock.Now().UTC()

	user, auth, err := s.verifyCert(ctx, certToken, now)
	if err != nil {
		return SelfEnrolment{}, err
	}
	if err := verifyPossession(proof, auth.DeviceKey, certToken, now); err != nil {
		return SelfEnrolment{}, err
	}

	existing, err := s.LookupDevice(ctx, auth.DeviceKey)
	switch {
	case err == nil:
		return existingDevice(existing, user, now)
	case !errors.Is(err, ErrUnknownDevice):
		return SelfEnrolment{}, err
	}

	device, err := s.EnrolDevice(ctx, certToken, name)
	if err == nil {
		return SelfEnrolment{Device: device, User: user, Created: true}, nil
	}
	// Two submissions of the same key raced; the row the other one wrote is the
	// answer, judged by the same rules a pre-existing one is.
	if errors.Is(err, ErrDeviceExists) {
		existing, lookupErr := s.LookupDevice(ctx, auth.DeviceKey)
		if lookupErr != nil {
			return SelfEnrolment{}, lookupErr
		}
		return existingDevice(existing, user, now)
	}
	return SelfEnrolment{}, err
}

// SelfEnrolment is what SelfEnrol reports: the device as enrolled, the pinned
// user the cert named, and whether this call created the row.
type SelfEnrolment struct {
	Device  Device
	User    User
	Created bool
}

// existingDevice is the idempotent branch: an enrolled, active device under the
// same user is handed back; anything else is a refusal.
func existingDevice(existing Device, user User, now time.Time) (SelfEnrolment, error) {
	if err := existing.Active(now); err != nil {
		return SelfEnrolment{}, err
	}
	if existing.UserID != user.ID {
		return SelfEnrolment{}, ErrCertMismatch
	}
	return SelfEnrolment{Device: existing, User: user, Created: false}, nil
}
