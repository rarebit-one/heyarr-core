package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Release intent for vault blobs (#779, ADR-0114).
//
// A vault blob is wanted by placement pins (its owner's devices, ADR-0096) and
// by placements taken from a sibling (ADR-0114). When the last of those goes,
// because the owner unpinned it or the sibling it came from withdrew it, nobody
// wants the bytes any more. On a two-site fabric GC's last-copy rule (ADR-0018)
// would still spare them forever at whichever site reclaims second, because no
// other peer claims them. The release row is the record that lets the personal
// profile's GC reclaim that last copy after the grace window (basis `released`).
//
// The row is written only for a vault blob: one no asset and no scanned library
// file reaches. Library media is never released, and the read side
// ([Catalog.Blobs]) re-checks scanned_files as well. Both transitions are
// events (blob.released, blob.release_cleared), so the log says why a blob
// became reclaimable and why it stopped being.

// Release causes, matching blob_releases.cause.
const (
	releaseUnpinned  = "unpinned"
	releaseWithdrawn = "withdrawn"
)

// Why a release was cleared, as blob.release_cleared names it.
const (
	wantedByPin       = "pinned"
	wantedByPlacement = "placement_taken"
	wantedByReference = "referenced"
)

// recordReleaseIfUnwanted records release intent for blobHash when nothing
// wants it any more: no placement pin, no taken placement, no asset and no
// scanned library file. It is called in the same transaction that removed a pin
// or a taken placement, so the check sees the removal. A blob something still
// wants records nothing.
//
// A recorded release also resets any older grace-window mark, so the window a
// release is honoured after always starts from the release.
func (c *Catalog) recordReleaseIfUnwanted(
	ctx context.Context, tx *sql.Tx, blobHash, cause, source, now string,
) ([]events.Event, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO blob_releases (blob_hash, cause, source_peer_id, released_at)
		SELECT ?, ?, ?, ?
		 WHERE NOT EXISTS (SELECT 1 FROM placement_pins WHERE blob_hash = ?)
		   AND NOT EXISTS (SELECT 1 FROM peer_placements WHERE blob_hash = ?)
		   AND NOT EXISTS (SELECT 1 FROM assets WHERE blob_hash = ?)
		   AND NOT EXISTS (SELECT 1 FROM scanned_files WHERE blob_hash = ?)
		ON CONFLICT (blob_hash) DO UPDATE SET
		   cause = excluded.cause, source_peer_id = excluded.source_peer_id,
		   released_at = excluded.released_at`,
		blobHash, cause, source, now, blobHash, blobHash, blobHash, blobHash)
	if err != nil {
		return nil, fmt.Errorf("catalog: recording the release of %s: %w", blobHash, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE blobs SET unreferenced_since = NULL WHERE hash = ? AND unreferenced_since IS NOT NULL`,
		blobHash); err != nil {
		return nil, fmt.Errorf("catalog: restarting the grace window on %s: %w", blobHash, err)
	}
	payload := map[string]any{"cause": cause, "released_at": now}
	if source != "" {
		payload["source_peer_id"] = source
	}
	ev, err := c.events.EmitTx(ctx, tx, events.TypeBlobReleased, "blob", blobHash, payload)
	if err != nil {
		return nil, err
	}
	return []events.Event{ev}, nil
}

// markWanted is a blob wanted again by a new pin or taken placement: its
// release no longer stands, and its grace window starts afresh the next time it
// goes unreferenced, rather than resuming a mark from before (#779).
func (c *Catalog) markWanted(ctx context.Context, tx *sql.Tx, blobHash, why string) ([]events.Event, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE blobs SET unreferenced_since = NULL WHERE hash = ? AND unreferenced_since IS NOT NULL`,
		blobHash); err != nil {
		return nil, fmt.Errorf("catalog: restarting the grace window on %s: %w", blobHash, err)
	}
	return c.clearRelease(ctx, tx, blobHash, why)
}

// clearRelease forgets release intent for a blob that is wanted again, and
// emits blob.release_cleared when there was one to forget.
func (c *Catalog) clearRelease(ctx context.Context, tx *sql.Tx, blobHash, why string) ([]events.Event, error) {
	n, err := deleteRelease(ctx, tx, blobHash)
	if err != nil || n == 0 {
		return nil, err
	}
	ev, err := c.events.EmitTx(ctx, tx, events.TypeBlobReleaseCleared, "blob", blobHash, map[string]any{"why": why})
	if err != nil {
		return nil, err
	}
	return []events.Event{ev}, nil
}

// deleteRelease removes a release row, reporting how many went.
func deleteRelease(ctx context.Context, tx *sql.Tx, blobHash string) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM blob_releases WHERE blob_hash = ?`, blobHash)
	if err != nil {
		return 0, fmt.Errorf("catalog: clearing the release of %s: %w", blobHash, err)
	}
	return res.RowsAffected()
}

// wantedAtReclaim is the reclaim transaction's own check that a tracked blob
// is still garbage (#779), independent of which evidence row sorts last:
//
//   - nothing may reach it: no placement pin, taken placement, scanned library
//     file or ready thumbnail. Assets are refused by ON DELETE RESTRICT.
//     Placement pins have no foreign key, so without this a pin added after
//     the sweep read the blob would not stop the delete;
//   - if THIS attempt's evidence (recorded at stamp) is the `released` basis,
//     the release must still stand AND be the generation whose grace window
//     the sweep honoured. A release resets the grace-window mark, so the mark
//     always postdates the release it is the window for. A release newer than
//     the mark (or a mark gone NULL) is a blob wanted and released again since
//     the sweep read it, and it gets its own full window.
func wantedAtReclaim(ctx context.Context, tx *sql.Tx, blobHash, stamp string) (bool, error) {
	var wanted, reliedOnRelease bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM placement_pins WHERE blob_hash = ?)
		    OR EXISTS (SELECT 1 FROM peer_placements WHERE blob_hash = ?)
		    OR EXISTS (SELECT 1 FROM scanned_files WHERE blob_hash = ?)
		    OR EXISTS (SELECT 1 FROM derived_thumbnails WHERE thumb_hash = ? AND status = 'ready'),
		       EXISTS (SELECT 1 FROM durability_evidence
		                WHERE blob_hash = ? AND basis = 'released' AND recorded_at = ?)`,
		blobHash, blobHash, blobHash, blobHash, blobHash, stamp).Scan(&wanted, &reliedOnRelease); err != nil {
		return false, fmt.Errorf("catalog: checking %s is still garbage before reclaiming it: %w", blobHash, err)
	}
	if wanted || !reliedOnRelease {
		return wanted, nil
	}

	var released, since sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT r.released_at, b.unreferenced_since
		  FROM blob_releases r JOIN blobs b ON b.hash = r.blob_hash
		 WHERE r.blob_hash = ?`, blobHash).Scan(&released, &since)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return true, nil // the release no longer stands
	case err != nil:
		return false, fmt.Errorf("catalog: reading the release of %s: %w", blobHash, err)
	case !released.Valid || !since.Valid:
		return true, nil // a release with no window started since: not this sweep's to honour
	}
	releasedAt, err1 := time.Parse(timestampFormat, released.String)
	markedAt, err2 := time.Parse(timestampFormat, since.String)
	if err1 != nil || err2 != nil {
		// Unreadable is not evidence: refuse rather than delete a last copy.
		return true, nil
	}
	return releasedAt.After(markedAt), nil
}
