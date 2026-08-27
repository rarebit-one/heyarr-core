package client

import (
	"context"
	"net/url"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

// The encrypted personal-state surface (§38, §42, ADR-0049): a device pushes the
// opaque things it minted client-side — a space, the wrapped copies of its key,
// and encrypted CRDT changes — to the peer over /api/v1, and fetches them back.
//
// As everywhere in this package the wire types are declared here rather than
// imported from the server, so a field renamed on the server surfaces as a
// failing test here (see the header of types.go). The one exception is
// protocol.EncryptedChange: it is not a server type but the shared opaque wire
// change both sides mint and verify by content-address, so both import it.

// WrappedKeyInput is one recipient's sealed copy of a space key, pushed at
// create time. Wrapped is opaque bytes (encryption.Seal output), base64 on the
// wire via encoding/json's []byte handling.
type WrappedKeyInput struct {
	Recipient string `json:"recipient"`
	Wrapped   []byte `json:"wrapped"`
}

// CreateSpaceRequest is the body of POST /spaces: a client-minted space and the
// wrapped copies of its key.
type CreateSpaceRequest struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	WrappedKeys []WrappedKeyInput `json:"wrapped_keys"`
}

// Space is a space as the peer holds it — the opaque id, the structural kind, and
// when the peer recorded it. No name: a name is encrypted state, not metadata.
type Space struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"created_at"`
}

type spacesEnvelope struct {
	Spaces []Space `json:"spaces"`
}

// WrappedKey is a stored wrapped copy a device fetches to find the one sealed for
// its own key. Wrapped is opaque base64 bytes.
type WrappedKey struct {
	Recipient string `json:"recipient"`
	Wrapped   []byte `json:"wrapped"`
	CreatedAt string `json:"created_at"`
}

type wrappedKeysEnvelope struct {
	SpaceID     string       `json:"space_id"`
	WrappedKeys []WrappedKey `json:"wrapped_keys"`
}

type changesEnvelope struct {
	SpaceID string                     `json:"space_id"`
	Changes []protocol.EncryptedChange `json:"changes"`
}

type changeStored struct {
	ChangeID string `json:"change_id"`
}

// CreateSpace pushes a client-minted space and its wrapped keys to the peer.
func (c *Client) CreateSpace(ctx context.Context, req CreateSpaceRequest) (Space, error) {
	var out Space
	if err := c.Post(ctx, "/spaces", req, &out); err != nil {
		return Space{}, err
	}
	return out, nil
}

// ListSpaces returns the spaces the peer holds (metadata only).
func (c *Client) ListSpaces(ctx context.Context) ([]Space, error) {
	var out spacesEnvelope
	if err := c.Get(ctx, "/spaces", nil, &out); err != nil {
		return nil, err
	}
	return out.Spaces, nil
}

// WrappedKeys returns the wrapped copies of a space's key — a device scans them
// for the recipient matching its own encryption key.
func (c *Client) WrappedKeys(ctx context.Context, spaceID string) ([]WrappedKey, error) {
	var out wrappedKeysEnvelope
	if err := c.Get(ctx, "/spaces/"+url.PathEscape(spaceID)+"/keys", nil, &out); err != nil {
		return nil, err
	}
	return out.WrappedKeys, nil
}

// PutChange pushes one encrypted change; the peer re-verifies its content-address
// before storing. Returns the id the peer holds it under.
func (c *Client) PutChange(ctx context.Context, ch protocol.EncryptedChange) (string, error) {
	var out changeStored
	if err := c.Post(ctx, "/spaces/"+url.PathEscape(ch.SpaceID)+"/changes", ch, &out); err != nil {
		return "", err
	}
	return out.ChangeID, nil
}

// Changes returns every encrypted change the peer holds for a space, oldest
// first — what a device pulls to decrypt and merge. Ciphertext throughout.
func (c *Client) Changes(ctx context.Context, spaceID string) ([]protocol.EncryptedChange, error) {
	var out changesEnvelope
	if err := c.Get(ctx, "/spaces/"+url.PathEscape(spaceID)+"/changes", nil, &out); err != nil {
		return nil, err
	}
	return out.Changes, nil
}

type snapshotStored struct {
	SnapshotID string `json:"snapshot_id"`
}

type compactResult struct {
	Dropped int `json:"dropped"`
}

// Snapshot fetches the latest encrypted snapshot for a space. ok is false (with a
// nil error) when the space has none yet.
func (c *Client) Snapshot(ctx context.Context, spaceID string) (protocol.EncryptedSnapshot, bool, error) {
	var out protocol.EncryptedSnapshot
	if err := c.Get(ctx, "/spaces/"+url.PathEscape(spaceID)+"/snapshot", nil, &out); err != nil {
		if IsNotFound(err) {
			return protocol.EncryptedSnapshot{}, false, nil
		}
		return protocol.EncryptedSnapshot{}, false, err
	}
	return out, true, nil
}

// PushSnapshot pushes a materialised, encrypted snapshot; the peer verifies its
// content-address before storing. Returns the id it holds it under.
func (c *Client) PushSnapshot(ctx context.Context, snap protocol.EncryptedSnapshot) (string, error) {
	var out snapshotStored
	if err := c.Post(ctx, "/spaces/"+url.PathEscape(snap.SpaceID)+"/snapshots", snap, &out); err != nil {
		return "", err
	}
	return out.SnapshotID, nil
}

// Compact drops the changes the latest snapshot subsumes and every replica holds
// (the acknowledged frontier), returning how many were dropped.
func (c *Client) Compact(ctx context.Context, spaceID string, frontier []string) (int, error) {
	var out compactResult
	body := map[string][]string{"frontier": frontier}
	if err := c.Post(ctx, "/spaces/"+url.PathEscape(spaceID)+"/compact", body, &out); err != nil {
		return 0, err
	}
	return out.Dropped, nil
}
