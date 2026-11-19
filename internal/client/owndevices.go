package client

import (
	"context"
	"net/url"
)

// The caller's own devices, as wrap targets for one space (ADR-0049). Pairing
// admits a new device to the user's identity but wraps it no space key; a
// device that holds the key reads this list, wraps the current key for the
// active devices lacking a copy, and pushes the copies with RewrapKeys. Needs
// a Device credential; a bearer is refused.

// The statuses an own device carries.
const (
	OwnDeviceActive          = "active"
	OwnDeviceRevoked         = "revoked"
	OwnDeviceNoEncryptionKey = "no_encryption_key"
)

// OwnDevice is one of the caller's user's enrolled devices.
type OwnDevice struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	DeviceKey string `json:"device_key"`
	// Recipient is the device's encryption key; empty for a device with none.
	Recipient       string `json:"recipient,omitempty"`
	Status          string `json:"status"`
	HoldsCurrentKey bool   `json:"holds_current_key"`
	ThisDevice      bool   `json:"this_device"`
	EnrolledAt      string `json:"enrolled_at"`
}

// OwnDevices is GET /spaces/{id}/own-devices: the space's current key epoch
// and every device of the caller's user, newest first.
type OwnDevices struct {
	SpaceID  string      `json:"space_id"`
	KeyEpoch int         `json:"key_epoch"`
	Devices  []OwnDevice `json:"devices"`
}

// SpaceOwnDevices lists the caller's user's devices and which of them hold a
// copy of the space's current key.
func (c *Client) SpaceOwnDevices(ctx context.Context, spaceID string) (OwnDevices, error) {
	var out OwnDevices
	if err := c.Get(ctx, "/spaces/"+url.PathEscape(spaceID)+"/own-devices", nil, &out); err != nil {
		return OwnDevices{}, err
	}
	return out, nil
}
