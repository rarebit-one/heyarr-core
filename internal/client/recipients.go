package client

import (
	"context"
	"net/url"
	"time"
)

// Service recipients and space grants (ADR-0104): an owner's device registers
// an executor's X25519 public key, then grants the executor a space, carrying
// the copy of the space's current key wrapped for that key. Every call here
// needs a Device credential carrying write; a bearer token is refused.

// ServiceRecipient is a registered executor key.
type ServiceRecipient struct {
	ID                 string `json:"id"`
	PrincipalID        string `json:"principal_id"`
	Recipient          string `json:"recipient"`
	Label              string `json:"label,omitempty"`
	Fingerprint        string `json:"fingerprint"`
	RegisteredByDevice string `json:"registered_by_device"`
	CreatedAt          string `json:"created_at"`
}

// RegisterServiceRecipientRequest is the body of POST /service-recipients.
// Fingerprint is the one the operator read off the executor's host; the peer
// refuses the registration when it is not the key's.
type RegisterServiceRecipientRequest struct {
	Principal   string `json:"principal"`
	Recipient   string `json:"recipient"`
	Label       string `json:"label,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// RemovedServiceRecipient acks a removal: the registration, and the spaces
// whose wrapped copy for the key was deleted with it.
type RemovedServiceRecipient struct {
	ServiceRecipient
	WrapsDeletedFrom []string `json:"wraps_deleted_from"`
}

type serviceRecipientsEnvelope struct {
	Recipients []ServiceRecipient `json:"recipients"`
}

// GrantRequest is the body of POST /spaces/{id}/grants. WrappedKeys carries the
// space's current key wrapped for the executor's service recipients; the peer
// records them and the grant in one transaction.
type GrantRequest struct {
	Principal   string            `json:"principal"`
	Caps        string            `json:"caps"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	WrappedKeys []WrappedKeyInput `json:"wrapped_keys,omitempty"`
}

// Grant is a recorded space grant.
type Grant struct {
	SpaceID         string   `json:"space_id"`
	PrincipalID     string   `json:"principal_id"`
	Caps            string   `json:"caps"`
	GrantedByDevice string   `json:"granted_by_device"`
	GrantedAt       string   `json:"granted_at"`
	ExpiresAt       string   `json:"expires_at,omitempty"`
	WrappedFor      []string `json:"wrapped_for,omitempty"`
}

// RegisterServiceRecipient registers an executor's public key as a wrap
// recipient. Registering a key already registered for the same executor
// returns that registration unchanged.
func (c *Client) RegisterServiceRecipient(ctx context.Context, req RegisterServiceRecipientRequest) (ServiceRecipient, error) {
	var out ServiceRecipient
	if err := c.Post(ctx, "/service-recipients", req, &out); err != nil {
		return ServiceRecipient{}, err
	}
	return out, nil
}

// ServiceRecipients lists the registered executor keys.
func (c *Client) ServiceRecipients(ctx context.Context) ([]ServiceRecipient, error) {
	var out serviceRecipientsEnvelope
	if err := c.Get(ctx, "/service-recipients", nil, &out); err != nil {
		return nil, err
	}
	return out.Recipients, nil
}

// RemoveServiceRecipient withdraws a registration, by id or by key; every
// wrapped copy held for the key on the peer goes with it.
func (c *Client) RemoveServiceRecipient(ctx context.Context, ref string) (RemovedServiceRecipient, error) {
	var out RemovedServiceRecipient
	if err := c.DeleteInto(ctx, "/service-recipients/"+url.PathEscape(ref), &out); err != nil {
		return RemovedServiceRecipient{}, err
	}
	return out, nil
}

// GrantSpace grants an executor access to a space, recording any wrapped copies
// in the same transaction.
func (c *Client) GrantSpace(ctx context.Context, spaceID string, req GrantRequest) (Grant, error) {
	var out Grant
	if err := c.Post(ctx, "/spaces/"+url.PathEscape(spaceID)+"/grants", req, &out); err != nil {
		return Grant{}, err
	}
	return out, nil
}

// RevokeSpaceGrant withdraws an executor's grant on a space and, in the same
// transaction, deletes the space key copies wrapped for its service recipients.
func (c *Client) RevokeSpaceGrant(ctx context.Context, spaceID, principal string) error {
	return c.Delete(ctx, "/spaces/"+url.PathEscape(spaceID)+"/grants/"+url.PathEscape(principal))
}
