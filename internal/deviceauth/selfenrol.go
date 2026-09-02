package deviceauth

import (
	"context"
	"errors"
)

// SelfEnrol enrols a device on the strength of its own credential (ADR-0067):
// its admitting op — a user-signed cert, or since ADR-0068 an add signed by
// any current member — a fresh possession proof, and the membership ops the
// device knows, with no admin in the loop.
//
// It is admissible because the facts the admin route relies on are all
// present in the request and all verifiable offline: the op, evaluated with
// the ops this node holds plus the ones presented, proves a member of the
// PINNED identity admitted this device key (ADR-0032's enrol-before-trust
// gate is the user pin, and it holds — an unpinned user's op is refused), and
// the proof proves the caller holds that key. The checks are the ones Verify
// makes on every authenticated request, in the same order and through the
// same code — under ADR-0068 the evaluation itself materialises the device's
// row, so what this call adds is the NAME and the created/existing answer.
//
// It is idempotent on the device key. A phone that lost the response and
// re-submits gets its existing row back (created=false) rather than a conflict.
// A REVOKED device is not re-enrolled — revocation is the admin's word against
// the log, and it wins — and an op naming a different user than the row holds
// is refused: neither is a path back in. What is granted is nothing beyond
// enrolment: the device authenticates at the read floor, and write stays an
// admin authorisation (ADR-0065).
func (s *Store) SelfEnrol(ctx context.Context, opToken, proof, name string, ops []string) (SelfEnrolment, error) {
	if opToken == "" || proof == "" {
		return SelfEnrolment{}, ErrMalformedCredential
	}
	now := s.clock.Now().UTC()

	op, err := parseOp(opToken)
	if err != nil {
		return SelfEnrolment{}, err
	}
	if err := verifyPossession(proof, op.Device, opToken, now); err != nil {
		return SelfEnrolment{}, err
	}
	// Whether the view held this device BEFORE the evaluation materialises it
	// is what created/existing reports.
	_, err = s.LookupDevice(ctx, op.Device)
	created := errors.Is(err, ErrUnknownDevice)
	if err != nil && !created {
		return SelfEnrolment{}, err
	}

	user, auth, err := s.verifyMembership(ctx, opToken, ops, now)
	if err != nil {
		return SelfEnrolment{}, err
	}
	device, err := s.memberDevice(ctx, user, auth.DeviceKey)
	if err != nil {
		return SelfEnrolment{}, err
	}
	if err := device.Active(now); err != nil {
		return SelfEnrolment{}, err
	}
	// A row the evaluation materialised (this call or an earlier authenticated
	// request) is unnamed; the first self-enrolment names it. That is the whole
	// of what an un-created answer changes — a renaming route is the admin's.
	if device.Name == "" && name != "" {
		if err := s.setDeviceName(ctx, device.ID, name); err != nil {
			return SelfEnrolment{}, err
		}
		device.Name = name
	}
	return SelfEnrolment{Device: device, User: user, Created: created}, nil
}

// SelfEnrolment is what SelfEnrol reports: the device as enrolled, the pinned
// user the op named, and whether this call created the row.
type SelfEnrolment struct {
	Device  Device
	User    User
	Created bool
}
