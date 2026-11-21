package personalstate

// memberwrite.go is the authority behind httpapi.RequireWriteOrMember
// (ADR-0128): may this Device credential write to this space as a member?
//
// A Device credential names its signing key; a space key is wrapped for the
// device's ENCRYPTION key. The authority joins the two through the device
// store — the device must be enrolled under this user, unrevoked, and carry
// an encryption key — and then asks the personal-state store whether that key
// holds the space's current key and the membership admits the user.

import (
	"context"
	"errors"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// DeviceLister lists a user's enrolled devices, revoked ones included.
// *deviceauth.Store satisfies it (as part of MemberResolver).
type DeviceLister interface {
	PrincipalDevices(ctx context.Context, principalID string) ([]deviceauth.Device, error)
}

// MemberWriteAuthority answers httpapi.MemberWrites over the personal-state
// store and the device store.
type MemberWriteAuthority struct {
	store   *store.Store
	devices DeviceLister
}

// NewMemberWriteAuthority builds the authority. Both are required: without
// either there is no way to say yes, and a route should refuse rather than
// guess.
func NewMemberWriteAuthority(st *store.Store, devices DeviceLister) (*MemberWriteAuthority, error) {
	if st == nil || devices == nil {
		return nil, errors.New("api/personalstate: member writes need the personal-state store and the device store")
	}
	return &MemberWriteAuthority{store: st, devices: devices}, nil
}

// MemberMayWrite reports whether the device deviceKey of user principalID may
// write to spaceID ("" for any space) as a member (ADR-0128).
func (m *MemberWriteAuthority) MemberMayWrite(ctx context.Context, principalID, deviceKey, spaceID string) (bool, error) {
	recipient, err := m.encryptionKey(ctx, principalID, deviceKey)
	if err != nil || recipient == "" {
		return false, err
	}
	if spaceID == "" {
		return m.store.MemberHoldsAnyCurrentKey(ctx, principalID, recipient)
	}
	return m.store.MemberHoldsCurrentKey(ctx, spaceID, principalID, recipient)
}

// encryptionKey is the encryption key of the user's enrolled, unrevoked device
// with this device key, or "" when there is none. Authentication has already
// refused a revoked device; this refuses it again, so a revocation landing
// mid-request cannot be outrun.
func (m *MemberWriteAuthority) encryptionKey(ctx context.Context, principalID, deviceKey string) (string, error) {
	if principalID == "" || deviceKey == "" {
		return "", nil
	}
	devices, err := m.devices.PrincipalDevices(ctx, principalID)
	if err != nil {
		return "", err
	}
	for _, d := range devices {
		if d.DeviceKey == deviceKey && d.RevokedAt == nil {
			return d.EncryptionKey, nil
		}
	}
	return "", nil
}

// compile-time: the authority is what the member-writable routes ask.
var _ httpapi.MemberWrites = (*MemberWriteAuthority)(nil)
