package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
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
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO placement_pins (blob_hash, peer_id, created_at) VALUES (?, ?, ?)
			 ON CONFLICT (blob_hash, peer_id) DO NOTHING`,
			blobHash, peerID, c.clock.Now().Format(timestampFormat)); err != nil {
			return fmt.Errorf("catalog: recording a placement pin: %w", err)
		}
		// Pinned again: whatever release it carried no longer stands (#779).
		evs, err := c.markWanted(ctx, tx, blobHash, wantedByPin)
		pending = evs
		return err
	})
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		c.events.Publish(pending...)
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
//
// present, when not nil, confirms the bytes are still in the store. It runs
// inside the transaction, which holds the write lock from its start, so it is
// serialized with GC's guarded unlink of a released blob (#779): either the
// unlink sees this row and keeps the bytes, or this sees the bytes gone and
// records nothing, returning ErrBytesGone for the uploader to retry.
func (c *Catalog) RecordVaultBlob(
	ctx context.Context, blobHash string, size int64, peerID string, present func(context.Context) error,
) error {
	return c.RecordVaultUpload(ctx, blobHash, size, peerID, UploadPin{}, present)
}

// UploadPin says what kind of pin a vault upload records (ADR-0096 amendment,
// #714). The zero value is the legacy permanent pin.
type UploadPin struct {
	// Uploader is the principal that uploaded the blob. It is recorded so a
	// restricted executor can later confirm only blobs it uploaded itself.
	Uploader string
	// ProvisionalUntil, when set, makes the pin provisional: kept until then,
	// and removed by [Catalog.ExpireProvisionalPins] unless the writer confirms
	// it first ([Catalog.ConfirmUploadPins]).
	ProvisionalUntil time.Time
}

// RecordVaultUpload is [Catalog.RecordVaultBlob] with an explicit upload pin.
//
// How a second upload of a blob already pinned to this peer combines with the
// pin it finds — every rule leans towards keeping bytes, because a pin that
// outlives its need costs storage and one that expires early loses a file:
//
//   - a permanent pin stays permanent (the amendment's dedupe rule);
//   - a permanent upload makes a provisional pin permanent (the compatibility
//     flag was switched off, or an older path wrote it);
//   - a provisional upload by the SAME principal extends the expiry;
//   - a provisional upload by a DIFFERENT principal makes the pin permanent.
//     Only one uploader is recorded per pin, so otherwise one of the two could
//     never confirm it, and its accepted write would lose the blob to expiry.
//     Sealing uses a random file id, so two writers producing identical
//     ciphertext is all but impossible; when it happens, a leak is the safe
//     side to err on.
func (c *Catalog) RecordVaultUpload(
	ctx context.Context, blobHash string, size int64, peerID string, pin UploadPin, present func(context.Context) error,
) error {
	if blobHash == "" || peerID == "" {
		return fmt.Errorf("catalog: a vault blob needs both a blob hash and a peer id")
	}
	now := c.clock.Now().UTC().Format(timestampFormat)
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		pending = nil
		if present != nil {
			if err := present(ctx); err != nil {
				return fmt.Errorf("%w: %s: %w", ErrBytesGone, blobHash, err)
			}
		}
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
		evs, err := c.recordUploadPin(ctx, tx, blobHash, peerID, pin)
		if err != nil {
			return err
		}
		pending = append(pending, evs...)
		evs, err = c.markWanted(ctx, tx, blobHash, wantedByPin)
		if err != nil {
			return err
		}
		pending = append(pending, evs...)
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

// recordUploadPin writes the upload's pin to this peer, combining it with any
// pin already there by the rules [Catalog.RecordVaultUpload] lists, and emits
// the transition when the pin's provisional state changed.
func (c *Catalog) recordUploadPin(
	ctx context.Context, tx *sql.Tx, blobHash, peerID string, pin UploadPin,
) ([]events.Event, error) {
	var (
		prevUntil, prevUploader sql.NullString
		exists                  = true
	)
	err := tx.QueryRowContext(ctx,
		`SELECT provisional_until, uploader FROM placement_pins WHERE blob_hash = ? AND peer_id = ?`,
		blobHash, peerID).Scan(&prevUntil, &prevUploader)
	if errors.Is(err, sql.ErrNoRows) {
		exists = false
	} else if err != nil {
		return nil, fmt.Errorf("catalog: reading the placement pin: %w", err)
	}

	until := ""
	if !pin.ProvisionalUntil.IsZero() {
		until = sqlite.FormatTimestamp(pin.ProvisionalUntil)
	}
	uploader := sql.NullString{String: pin.Uploader, Valid: pin.Uploader != ""}

	if !exists {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO placement_pins (blob_hash, peer_id, created_at, provisional_until, uploader)
			 VALUES (?, ?, ?, ?, ?)`,
			blobHash, peerID, c.clock.Now().Format(timestampFormat),
			sql.NullString{String: until, Valid: until != ""}, uploader); err != nil {
			return nil, fmt.Errorf("catalog: recording a placement pin: %w", err)
		}
		if until == "" {
			return nil, nil
		}
		return c.emitPinTransition(ctx, tx, events.TypePinProvisional, blobHash, peerID, map[string]any{
			"provisional_until": until,
		})
	}

	switch {
	case !prevUntil.Valid:
		return nil, nil // permanent stays permanent
	case until == "" || prevUploader != uploader:
		reason := "permanent_upload"
		if until != "" {
			reason = "second_uploader"
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE placement_pins SET provisional_until = NULL WHERE blob_hash = ? AND peer_id = ?`,
			blobHash, peerID); err != nil {
			return nil, fmt.Errorf("catalog: making a placement pin permanent: %w", err)
		}
		return c.emitPinTransition(ctx, tx, events.TypePinConfirmed, blobHash, peerID, map[string]any{
			"reason": reason,
		})
	case until > prevUntil.String:
		if _, err := tx.ExecContext(ctx,
			`UPDATE placement_pins SET provisional_until = ? WHERE blob_hash = ? AND peer_id = ?`,
			until, blobHash, peerID); err != nil {
			return nil, fmt.Errorf("catalog: extending a provisional pin: %w", err)
		}
		return c.emitPinTransition(ctx, tx, events.TypePinProvisional, blobHash, peerID, map[string]any{
			"provisional_until": until,
		})
	}
	return nil, nil
}

func (c *Catalog) emitPinTransition(
	ctx context.Context, tx *sql.Tx, eventType, blobHash, peerID string, payload map[string]any,
) ([]events.Event, error) {
	payload["peer_id"] = peerID
	ev, err := c.events.EmitTx(ctx, tx, eventType, "blob", blobHash, payload)
	if err != nil {
		return nil, err
	}
	return []events.Event{ev}, nil
}

// ConfirmResult is the outcome of confirming one blob's upload pin.
type ConfirmResult struct {
	BlobHash string
	// Status is ConfirmConfirmed, ConfirmAlreadyPermanent, ConfirmNotHeld or
	// ConfirmNotUploader.
	Status string
}

// Confirm outcomes, as POST /vault/blobs/confirm reports them per blob.
const (
	ConfirmConfirmed        = "confirmed"
	ConfirmAlreadyPermanent = "already_permanent"
	ConfirmNotHeld          = "not_held"
	ConfirmNotUploader      = "not_uploader"
)

// ConfirmUploadPins makes the upload pins on peerID for blobs permanent: the
// writer's drive change naming them was accepted (ADR-0096 amendment, #714).
//
// Confirming is idempotent and never removes anything, so it is not a
// destructive right: a confused or hostile caller can at worst keep bytes it
// could already have uploaded. A restricted executor (restricted) may confirm
// only pins whose recorded uploader is its own principal; any other caller may
// confirm any pin it names. Each blob is answered on its own: one unknown blob
// does not stop the rest.
func (c *Catalog) ConfirmUploadPins(
	ctx context.Context, peerID string, blobs []string, principal string, restricted bool,
) ([]ConfirmResult, error) {
	out := make([]ConfirmResult, 0, len(blobs))
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		out, pending = out[:0], nil
		for _, blob := range blobs {
			var until, uploader sql.NullString
			err := tx.QueryRowContext(ctx,
				`SELECT provisional_until, uploader FROM placement_pins WHERE blob_hash = ? AND peer_id = ?`,
				blob, peerID).Scan(&until, &uploader)
			status := ConfirmConfirmed
			switch {
			case errors.Is(err, sql.ErrNoRows):
				status = ConfirmNotHeld
			case err != nil:
				return fmt.Errorf("catalog: reading the pin on %s: %w", blob, err)
			case !until.Valid:
				status = ConfirmAlreadyPermanent
			case restricted && uploader.String != principal:
				status = ConfirmNotUploader
			}
			out = append(out, ConfirmResult{BlobHash: blob, Status: status})
			if status != ConfirmConfirmed {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE placement_pins SET provisional_until = NULL WHERE blob_hash = ? AND peer_id = ?`,
				blob, peerID); err != nil {
				return fmt.Errorf("catalog: confirming the pin on %s: %w", blob, err)
			}
			evs, err := c.emitPinTransition(ctx, tx, events.TypePinConfirmed, blob, peerID, map[string]any{
				"reason": "confirmed",
			})
			if err != nil {
				return err
			}
			pending = append(pending, evs...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		c.events.Publish(pending...)
	}
	return out, nil
}

// expireBatch bounds one expiry transaction, so a backlog never holds the
// writer for long.
const expireBatch = 200

// ExpireProvisionalPins removes every provisional pin whose expiry is before
// now, and returns how many it removed (ADR-0096 amendment, #714).
//
// This is how GC comes to treat an expired provisional pin as absent: the row
// goes, and the blob is released exactly as an explicit unpin releases it —
// recorded only if nothing else still wants it (another peer's pin, a taken
// placement, an asset), with GC's grace window still to run before any byte is
// unlinked (invariant 8). A pin confirmed in the meantime is permanent and is
// never matched.
func (c *Catalog) ExpireProvisionalPins(ctx context.Context, now time.Time) (int, error) {
	cutoff := sqlite.FormatTimestamp(now)
	stamp := now.UTC().Format(timestampFormat)
	total := 0
	for {
		var pending []events.Event
		n := 0
		err := c.db.InTx(ctx, func(tx *sql.Tx) error {
			pending, n = nil, 0
			batch, err := expiredPins(ctx, tx, cutoff)
			if err != nil {
				return err
			}
			for _, e := range batch {
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM placement_pins WHERE blob_hash = ? AND peer_id = ?`, e.blob, e.peer); err != nil {
					return fmt.Errorf("catalog: removing an expired provisional pin: %w", err)
				}
				evs, err := c.emitPinTransition(ctx, tx, events.TypePinExpired, e.blob, e.peer, map[string]any{
					"provisional_until": e.until,
				})
				if err != nil {
					return err
				}
				pending = append(pending, evs...)
				evs, err = c.recordReleaseIfUnwanted(ctx, tx, e.blob, releaseUnpinned, "", stamp)
				if err != nil {
					return err
				}
				pending = append(pending, evs...)
			}
			n = len(batch)
			return nil
		})
		if err != nil {
			return total, err
		}
		if len(pending) > 0 {
			c.events.Publish(pending...)
		}
		total += n
		if n < expireBatch {
			return total, nil
		}
	}
}

type expiredPin struct{ blob, peer, until string }

// expiredPins reads one batch of provisional pins whose expiry is before
// cutoff, oldest first.
func expiredPins(ctx context.Context, tx *sql.Tx, cutoff string) ([]expiredPin, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT blob_hash, peer_id, provisional_until FROM placement_pins
		  WHERE provisional_until IS NOT NULL AND provisional_until < ?
		  ORDER BY provisional_until LIMIT ?`, cutoff, expireBatch)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing expired provisional pins: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var batch []expiredPin
	for rows.Next() {
		var e expiredPin
		if err := rows.Scan(&e.blob, &e.peer, &e.until); err != nil {
			return nil, err
		}
		batch = append(batch, e)
	}
	return batch, rows.Err()
}

// ErrBytesGone is RecordVaultBlob or RecordBlobTransferred finding the bytes
// it was asked to record no longer in the store: GC reclaimed a released blob
// of the same hash between the store write and this commit (#779). Nothing was
// recorded; the upload or transfer should be retried.
var ErrBytesGone = errors.New("catalog: the blob's bytes are no longer in the store")

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
//
// Removing the LAST thing that wants a vault blob records release intent in the
// same transaction (#779, ADR-0114), so the personal profile's GC may reclaim
// its last copy after the grace window.
func (c *Catalog) UnpinPlacement(ctx context.Context, blobHash, peerID string) error {
	now := c.clock.Now().UTC().Format(timestampFormat)
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		pending = nil
		res, err := tx.ExecContext(ctx,
			`DELETE FROM placement_pins WHERE blob_hash = ? AND peer_id = ?`, blobHash, peerID)
		if err != nil {
			return fmt.Errorf("catalog: removing a placement pin: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		pending, err = c.recordReleaseIfUnwanted(ctx, tx, blobHash, releaseUnpinned, "", now)
		return err
	})
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		c.events.Publish(pending...)
	}
	return nil
}

// IsVaultBlob reports whether a blob is a vault blob: pinned to some peer
// (ADR-0096) or taken from a sibling site (ADR-0114), and referenced by no
// asset and no scanned library file. It is what
// confines a restricted principal's blob reads to ciphertext (ADR-0104): media
// is never a vault blob, even if someone pins its hash, because media is always
// reachable from an `assets` or `scanned_files` row.
func (c *Catalog) IsVaultBlob(ctx context.Context, blobHash string) (bool, error) {
	var ok bool
	err := c.db.Reader().QueryRowContext(ctx, `
		SELECT (EXISTS (SELECT 1 FROM placement_pins WHERE blob_hash = ?)
		        OR EXISTS (SELECT 1 FROM peer_placements WHERE blob_hash = ?))
		   AND NOT EXISTS (SELECT 1 FROM assets WHERE blob_hash = ?)
		   AND NOT EXISTS (SELECT 1 FROM scanned_files WHERE blob_hash = ?)`,
		blobHash, blobHash, blobHash, blobHash).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("catalog: checking whether %s is a vault blob: %w", blobHash, err)
	}
	return ok, nil
}
