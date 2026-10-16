package client

import (
	"context"
	"net/url"
)

// Shared-space membership (ADR-0108): who a space is shared with. Adding a
// member is two steps — this records the membership and names the member's
// enrolled device keys; the caller's device then wraps the space's current key
// for each of them with RewrapKeys. Removing one is a tombstone that names the
// recipients the rotation making it forward-secret must leave out. The adds
// and removes need a Device credential carrying write; a bearer is refused.

// SpaceMember is one user's membership of a space, current or removed.
type SpaceMember struct {
	SpaceID       string `json:"space_id"`
	PrincipalID   string `json:"principal_id"`
	Role          string `json:"role"`
	AddedAt       string `json:"added_at"`
	AddedByDevice string `json:"added_by_device,omitempty"`
	// RemovedAt is set on a tombstone: the user was a member until then.
	RemovedAt string `json:"removed_at,omitempty"`
}

// Current reports whether the membership is in force.
func (m SpaceMember) Current() bool { return m.RemovedAt == "" }

// AddMemberRequest is the body of POST /spaces/{id}/members: the user, by
// principal id or name, or user identity id.
type AddMemberRequest struct {
	User string `json:"user"`
}

// MemberAdded acks an add: the row, the space's current key epoch and the
// member's enrolled device keys to wrap the key for, at that epoch.
type MemberAdded struct {
	SpaceMember
	KeyEpoch int      `json:"key_epoch"`
	WrapFor  []string `json:"wrap_for"`
}

// MemberRemoved acks a removal: the tombstoned row, the space's current key
// epoch and the removed member's device keys that hold a copy of the current
// key — the revoke list of the rotation that completes the removal.
type MemberRemoved struct {
	SpaceMember
	Revoke   []string `json:"revoke"`
	KeyEpoch int      `json:"key_epoch"`
}

type membersEnvelope struct {
	SpaceID string        `json:"space_id"`
	Members []SpaceMember `json:"members"`
}

// SpaceMembers lists a space's membership rows, current and removed.
func (c *Client) SpaceMembers(ctx context.Context, spaceID string) ([]SpaceMember, error) {
	var out membersEnvelope
	if err := c.Get(ctx, "/spaces/"+url.PathEscape(spaceID)+"/members", nil, &out); err != nil {
		return nil, err
	}
	return out.Members, nil
}

// AddSpaceMember records user as a member of the space and returns the device
// keys to wrap the space key for.
func (c *Client) AddSpaceMember(ctx context.Context, spaceID, user string) (MemberAdded, error) {
	var out MemberAdded
	if err := c.Post(ctx, "/spaces/"+url.PathEscape(spaceID)+"/members", AddMemberRequest{User: user}, &out); err != nil {
		return MemberAdded{}, err
	}
	return out, nil
}

// RemoveSpaceMember tombstones user's membership of the space and returns the
// recipients a rotation must leave out.
func (c *Client) RemoveSpaceMember(ctx context.Context, spaceID, user string) (MemberRemoved, error) {
	var out MemberRemoved
	if err := c.DeleteInto(ctx, "/spaces/"+url.PathEscape(spaceID)+"/members/"+url.PathEscape(user), &out); err != nil {
		return MemberRemoved{}, err
	}
	return out, nil
}
