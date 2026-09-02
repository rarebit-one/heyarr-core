package client

import (
	"context"
	"net/url"
	"time"
)

// IdentityDevice is a device key a pinned user has vouched for, as the admin
// surface renders it (§40, ADR-0048, ADR-0068).
type IdentityDevice struct {
	ID            string     `json:"id"`
	UserID        string     `json:"user_id"`
	DeviceKey     string     `json:"device_key"`
	EncryptionKey string     `json:"encryption_key,omitempty"`
	Name          string     `json:"name"`
	AdmittedBy    string     `json:"admitted_by"`
	AdmittingOp   string     `json:"admitting_op"`
	EnrolledAt    time.Time  `json:"enrolled_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	RevokedAt     *time.Time `json:"revoked_at"`
}

// RevokeIdentityDevice tombstones one device on the peer (the admin's local
// word, not a membership op — ADR-0068) and returns the record it revoked,
// including the encryption key an operator re-keys spaces away from.
// Idempotent: revoking an already-revoked device returns it unchanged.
func (c *Client) RevokeIdentityDevice(ctx context.Context, deviceKey string) (IdentityDevice, error) {
	var out IdentityDevice
	if err := c.DeleteInto(ctx, "/identities/devices/"+url.PathEscape(deviceKey), &out); err != nil {
		return IdentityDevice{}, err
	}
	return out, nil
}
