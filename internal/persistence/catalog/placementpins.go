package catalog

import (
	"context"
	"fmt"
)

// PinPlacement records an opaque placement pin: a ciphertext blob should live on
// a peer (ADR-0096). It carries no space, path or asset link, so the control
// plane learns only that this blob belongs on this peer — never which vault or
// file it is. A pin keeps a vault blob (which has no `assets` row) from garbage
// collection — [Catalog.Blobs] counts it as a reference — and marks the blob
// desired on that peer for the replication pipeline. Idempotent per (blob, peer).
//
// There is no FK check: a pin may be recorded before the bytes are present and
// must outlive churn in `blobs`/`peers` (migration 00050).
func (c *Catalog) PinPlacement(ctx context.Context, blobHash, peerID string) error {
	if blobHash == "" || peerID == "" {
		return fmt.Errorf("catalog: a placement pin needs both a blob hash and a peer id")
	}
	_, err := c.db.Writer().ExecContext(ctx,
		`INSERT INTO placement_pins (blob_hash, peer_id, created_at) VALUES (?, ?, ?)
		 ON CONFLICT (blob_hash, peer_id) DO NOTHING`,
		blobHash, peerID, c.clock.Now().Format(timestampFormat))
	if err != nil {
		return fmt.Errorf("catalog: recording a placement pin: %w", err)
	}
	return nil
}

// PlacementPin is one recorded pin: a ciphertext blob a peer should hold
// (ADR-0096). It is the (blob, peer) pair and nothing else — the table carries a
// created_at, but a placement decision does not depend on when the pin was made.
type PlacementPin struct {
	BlobHash string
	PeerID   string
}

// AllPlacementPins is every placement pin, in deterministic order.
//
// It is what convergence unions on top of the canonical-set diff: a pin says a
// blob belongs on a peer even when no live asset does (a vault blob has no
// `assets` row, ADR-0096), so the pins are the second source of "what a peer
// should hold" that PlanPeerConvergence consults after replication.Diff. Ordered
// by blob then peer so the union it feeds stays as deterministic as the diff.
func (c *Catalog) AllPlacementPins(ctx context.Context) ([]PlacementPin, error) {
	rows, err := c.db.Reader().QueryContext(ctx,
		`SELECT blob_hash, peer_id FROM placement_pins ORDER BY blob_hash, peer_id`)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading the placement pins: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PlacementPin
	for rows.Next() {
		var p PlacementPin
		if err := rows.Scan(&p.BlobHash, &p.PeerID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UnpinPlacement removes a placement pin, so the pinned peer no longer keeps the
// blob or is a replication target for it. The device calls this when the drive
// CRDT's retention lets the blob go (ADR-0096); dropping a pin that is not there
// is not an error.
func (c *Catalog) UnpinPlacement(ctx context.Context, blobHash, peerID string) error {
	_, err := c.db.Writer().ExecContext(ctx,
		`DELETE FROM placement_pins WHERE blob_hash = ? AND peer_id = ?`, blobHash, peerID)
	if err != nil {
		return fmt.Errorf("catalog: removing a placement pin: %w", err)
	}
	return nil
}
