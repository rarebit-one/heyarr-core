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
//
// Epoch is the key epoch the copy seals (ADR-0103); zero — the space's original
// key — is omitted from the wire. The peer refuses a copy that is not at the
// space's current epoch with a 409.
type WrappedKeyInput struct {
	Recipient string `json:"recipient"`
	Wrapped   []byte `json:"wrapped"`
	Epoch     int    `json:"epoch,omitempty"`
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
	// Epoch is the key epoch this copy seals (ADR-0103).
	Epoch     int    `json:"epoch"`
	CreatedAt string `json:"created_at"`
}

// SpaceKeys is GET /spaces/{id}/keys whole: the wrapped copies and the space's
// current key epoch (ADR-0103) — the epoch a device wraps at to add a recipient,
// and the one a rotation names as its expected epoch.
type SpaceKeys struct {
	SpaceID     string       `json:"space_id"`
	KeyEpoch    int          `json:"key_epoch"`
	WrappedKeys []WrappedKey `json:"wrapped_keys"`
}

// KeyHistoryEntry is one link of a space's key chain (ADR-0103): the key of
// epoch Epoch-1 sealed under the key of epoch Epoch. SealedPrev is opaque.
type KeyHistoryEntry struct {
	Epoch      int    `json:"epoch"`
	SealedPrev []byte `json:"sealed_prev"`
	CreatedAt  string `json:"created_at"`
}

type keyHistoryEnvelope struct {
	SpaceID string            `json:"space_id"`
	Entries []KeyHistoryEntry `json:"entries"`
}

// rotateRequest is the body of POST /spaces/{id}/rotate.
type rotateRequest struct {
	ExpectedEpoch int               `json:"expected_epoch"`
	SealedPrev    []byte            `json:"sealed_prev"`
	WrappedKeys   []WrappedKeyInput `json:"wrapped_keys"`
	Revoke        []string          `json:"revoke"`
}

type rotateResult struct {
	SpaceID  string `json:"space_id"`
	KeyEpoch int    `json:"key_epoch"`
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
	keys, err := c.SpaceKeys(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	return keys.WrappedKeys, nil
}

// SpaceKeys returns a space's wrapped copies together with its current key
// epoch (ADR-0103).
func (c *Client) SpaceKeys(ctx context.Context, spaceID string) (SpaceKeys, error) {
	var out SpaceKeys
	if err := c.Get(ctx, "/spaces/"+url.PathEscape(spaceID)+"/keys", nil, &out); err != nil {
		return SpaceKeys{}, err
	}
	return out, nil
}

// KeyHistory returns a space's opaque key chain, oldest epoch first (ADR-0103).
// A space never rotated has none.
func (c *Client) KeyHistory(ctx context.Context, spaceID string) ([]KeyHistoryEntry, error) {
	var out keyHistoryEnvelope
	if err := c.Get(ctx, "/spaces/"+url.PathEscape(spaceID)+"/key-history", nil, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// RotateKey moves a space to its next key epoch (ADR-0103): sealedPrev is the
// current key sealed under the new one, wraps the new key's copies (their Epoch
// is left zero — the rotation assigns it), and revoke the current recipients
// left out, who lose their copy. It is a compare-and-swap on expectedEpoch AND
// on the recipient set (#703): wraps plus revoke must name exactly the
// recipients holding a current copy, or it is a 409 — as is losing a race to
// another rotation. Needs `admin`. Returns the new epoch.
func (c *Client) RotateKey(ctx context.Context, spaceID string, expectedEpoch int, sealedPrev []byte, wraps []WrappedKeyInput, revoke []string) (int, error) {
	var out rotateResult
	if revoke == nil {
		revoke = []string{} // the field is required; null is not an empty set
	}
	req := rotateRequest{ExpectedEpoch: expectedEpoch, SealedPrev: sealedPrev, WrappedKeys: wraps, Revoke: revoke}
	if err := c.Post(ctx, "/spaces/"+url.PathEscape(spaceID)+"/rotate", req, &out); err != nil {
		return 0, err
	}
	return out.KeyEpoch, nil
}

// rewrapRequest is the body of POST /spaces/{id}/keys: wrapped copies of a
// space's current key, each naming the epoch it seals (ADR-0103).
type rewrapRequest struct {
	WrappedKeys []WrappedKeyInput `json:"wrapped_keys"`
}

// RewrapKeys replaces the wrapped copies of a space's key after a rotation — the
// remaining recipients' copies now seal the new key. The peer upserts each.
func (c *Client) RewrapKeys(ctx context.Context, spaceID string, keys []WrappedKeyInput) error {
	return c.Post(ctx, "/spaces/"+url.PathEscape(spaceID)+"/keys", rewrapRequest{WrappedKeys: keys}, nil)
}

// RevokeKey deletes one recipient's wrapped copy of a space's key — the storage
// half of revocation. Idempotent: revoking a copy already gone is not an error.
func (c *Client) RevokeKey(ctx context.Context, spaceID, recipient string) error {
	return c.Delete(ctx, "/spaces/"+url.PathEscape(spaceID)+"/keys/"+url.PathEscape(recipient))
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
