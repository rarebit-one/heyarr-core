package catalog

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/events"
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

// RecordVaultBlob records a vault blob this node has just stored and verified:
// its `blobs` row, this node's `present` replica, and the placement pin that
// retains it (ADR-0096) — in one transaction, so the three cannot disagree.
//
// # Why a vault blob needs a row at all (#658)
//
// Until #658 the vault upload wrote only the pin. The bytes were in the store
// and nothing in the catalogue said so, which went wrong twice:
//
//   - Convergence read the self-pin as a gap — this node pinned, holding
//     nothing it had recorded — and queued replicate_blob for it. The handler
//     found the bytes already held and tried to record the replica, and
//     `replicas.blob_hash` references `blobs`, so the insert was refused with a
//     FOREIGN KEY failure. Every cycle, for every vault blob, forever.
//   - Garbage collection walks `blobs` for tracked bytes and treats everything
//     else in the store as untracked orphans, which a pin does not protect.
//
// The row carries a hash and a size, both of which the pin and the store
// already reveal: it tells the control plane nothing about which vault or file
// the bytes belong to, which is the leak ADR-0096 exists to deny. There is
// still no `assets` row.
//
// Idempotent (invariant 9): a re-upload of bytes already recorded writes
// nothing and emits nothing.
func (c *Catalog) RecordVaultBlob(ctx context.Context, blobHash string, size int64, peerID string) error {
	if blobHash == "" || peerID == "" {
		return fmt.Errorf("catalog: a vault blob needs both a blob hash and a peer id")
	}
	now := c.clock.Now().UTC().Format(timestampFormat)
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		pending = nil
		adopted, err := adoptBlob(ctx, tx, blobHash, size, now)
		if err != nil {
			return err
		}
		if adopted {
			ev, err := c.events.EmitTx(ctx, tx, events.TypeBlobCreated, "blob", blobHash, map[string]any{
				"size": size, "vault": true,
			})
			if err != nil {
				return err
			}
			pending = append(pending, ev)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, updated_at)
			VALUES (?, ?, 'present', ?, ?, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = 'present', bytes_present = excluded.bytes_present,
				verified_at = excluded.verified_at, updated_at = excluded.updated_at
			WHERE replicas.state <> 'present' OR replicas.bytes_present <> excluded.bytes_present`,
			blobHash, peerID, size, now, now)
		if err != nil {
			return fmt.Errorf("catalog: recording the vault replica of %s on peer %s: %w", blobHash, peerID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("catalog: recording the vault replica of %s on peer %s: %w", blobHash, peerID, err)
		}
		if n == 1 {
			ev, err := c.events.EmitTx(ctx, tx, events.TypeReplicaPresent, "blob", blobHash, map[string]any{
				"peer_id": peerID, "bytes_present": size,
			})
			if err != nil {
				return err
			}
			pending = append(pending, ev)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO placement_pins (blob_hash, peer_id, created_at) VALUES (?, ?, ?)
			 ON CONFLICT (blob_hash, peer_id) DO NOTHING`,
			blobHash, peerID, c.clock.Now().Format(timestampFormat)); err != nil {
			return fmt.Errorf("catalog: recording a placement pin: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		c.events.Publish(pending...)
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
