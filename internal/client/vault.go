package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/rarebit-one/void-which-binds-go/hashing"
)

// The vault WRITE surface (ADR-0021, ADR-0096): a device uploads pre-encrypted,
// content-addressed ciphertext blobs and records the placement pins that keep
// them. Reads reuse the shared, honest GET /blobs/{hash}/content contract
// (blobs.go) — the vault-specific half is only the write, because the plaintext
// never exists on the peer and the drive-CRDT reference that would retain a blob
// is encrypted state the peer cannot see.

// PutVaultBlob uploads a ciphertext blob under its blake3 id to
// PUT /api/v1/vault/blobs/{hash}. The peer re-verifies the bytes hash to the id
// the caller declares, stores them, and self-pins the blob to this node so GC
// retains it (ADR-0096). A body that does not hash to the declared id is a 400,
// surfaced here with the id so a caller can tell a corrupt upload from a wrong id.
//
// It uses the streaming transport (no request timeout): a vault blob is media and
// legitimately larger than any deadline worth setting, exactly as a blob READ is.
func (c *Client) PutVaultBlob(ctx context.Context, hash string, r io.Reader) error {
	parsed, err := hashing.Parse(hash)
	if err != nil {
		// Refused here rather than sent: nothing unvalidated becomes a URL path,
		// and the server would only answer the same after a round trip.
		return fmt.Errorf("%q is not a blob identifier — it must be blake3:<64 lowercase hex characters>", hash)
	}
	req, err := c.newRequest(ctx, http.MethodPut, "/vault/blobs/"+parsed.String(), nil, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.do(c.stream, req)
	if err != nil {
		// do() already renders a 400 as the peer's own detail ("the uploaded bytes
		// do not hash to …"); name the blob so the failure is anchored to which one.
		return fmt.Errorf("uploading vault blob %s: %w", parsed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// ConfirmResult is one blob's answer to [Client.ConfirmVaultBlobs].
type ConfirmResult struct {
	Blob   string `json:"blob"`
	Status string `json:"status"`
}

// ConfirmVaultBlobs makes this node's upload pins on blobs permanent via
// POST /api/v1/vault/blobs/confirm (ADR-0096 amendment, #714). A writer calls
// it once the drive change naming the blobs was accepted, never before: on a
// node with provisional upload pins on, an unconfirmed upload is released to
// GC after a day.
//
// A node that predates the route answers 404, and its upload pins are all
// permanent, so there is nothing to confirm: that is reported as no results
// and no error, so a newer client keeps working against an older node.
func (c *Client) ConfirmVaultBlobs(ctx context.Context, blobs []string) ([]ConfirmResult, error) {
	for _, b := range blobs {
		if _, err := hashing.Parse(b); err != nil {
			return nil, fmt.Errorf("%q is not a blob identifier — it must be blake3:<64 lowercase hex characters>", b)
		}
	}
	var out struct {
		Results []ConfirmResult `json:"results"`
	}
	if err := c.Post(ctx, "/vault/blobs/confirm", map[string][]string{"blobs": blobs}, &out); err != nil {
		var apiErr *Error
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("confirming vault blobs: %w", err)
	}
	return out.Results, nil
}

// PinPlacement records a placement pin — an opaque (blob, peer) pair — via
// POST /api/v1/vault/placements, so a ciphertext vault blob is retained on a peer
// OTHER than the one it was uploaded to and becomes a replication destination for
// it (ADR-0096). Idempotent per pair: re-pinning is a success, not a conflict.
func (c *Client) PinPlacement(ctx context.Context, blobHash, peerID string) error {
	if _, err := hashing.Parse(blobHash); err != nil {
		return fmt.Errorf("%q is not a blob identifier — it must be blake3:<64 lowercase hex characters>", blobHash)
	}
	body := map[string]string{"blob_hash": blobHash, "peer_id": peerID}
	return c.Post(ctx, "/vault/placements", body, nil)
}
