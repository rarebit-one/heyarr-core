package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// ([Catalog.Blobs]) re-checks scanned_files as well.

// Release causes, matching blob_releases.cause.
const (
	releaseUnpinned  = "unpinned"
	releaseWithdrawn = "withdrawn"
)

// recordReleaseIfUnwanted records release intent for blobHash when nothing
// wants it any more: no placement pin, no taken placement, no asset and no
// scanned library file. It is called in the same transaction that removed a pin
// or a taken placement, so the check sees the removal. A blob something still
// wants records nothing.
func recordReleaseIfUnwanted(ctx context.Context, tx *sql.Tx, blobHash, cause, source, now string) error {
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
		return fmt.Errorf("catalog: recording the release of %s: %w", blobHash, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	// The grace window a release is honoured after starts from the release,
	// never from an older mark left by an earlier unreferenced spell.
	if _, err := tx.ExecContext(ctx,
		`UPDATE blobs SET unreferenced_since = NULL WHERE hash = ? AND unreferenced_since IS NOT NULL`,
		blobHash); err != nil {
		return fmt.Errorf("catalog: restarting the grace window on %s: %w", blobHash, err)
	}
	return nil
}

// markWanted is a blob wanted again by a new pin or taken placement: its
// release no longer stands, and its grace window starts afresh the next time it
// goes unreferenced, rather than resuming a mark from before (#779).
func markWanted(ctx context.Context, tx *sql.Tx, blobHash string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE blobs SET unreferenced_since = NULL WHERE hash = ? AND unreferenced_since IS NOT NULL`,
		blobHash); err != nil {
		return fmt.Errorf("catalog: restarting the grace window on %s: %w", blobHash, err)
	}
	return clearRelease(ctx, tx, blobHash)
}

// staleRelease reports whether a blob whose latest durability evidence is the
// `released` basis may NOT be reclaimed after all: its release is gone, or a
// pin, taken placement, scanned library file or ready thumbnail reaches it.
// Evidence on any other basis is not this check's business, and reports false.
func staleRelease(ctx context.Context, tx *sql.Tx, blobHash string) (bool, error) {
	var basis string
	err := tx.QueryRowContext(ctx, `
		SELECT basis FROM durability_evidence WHERE blob_hash = ?
		 ORDER BY recorded_at DESC, id DESC LIMIT 1`, blobHash).Scan(&basis)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("catalog: reading the durability basis for %s: %w", blobHash, err)
	case basis != "released":
		return false, nil
	}
	var stale bool
	if err := tx.QueryRowContext(ctx, `
		SELECT NOT EXISTS (SELECT 1 FROM blob_releases WHERE blob_hash = ?)
		    OR EXISTS (SELECT 1 FROM placement_pins WHERE blob_hash = ?)
		    OR EXISTS (SELECT 1 FROM peer_placements WHERE blob_hash = ?)
		    OR EXISTS (SELECT 1 FROM scanned_files WHERE blob_hash = ?)
		    OR EXISTS (SELECT 1 FROM derived_thumbnails WHERE thumb_hash = ? AND status = 'ready')`,
		blobHash, blobHash, blobHash, blobHash, blobHash).Scan(&stale); err != nil {
		return false, fmt.Errorf("catalog: revalidating the release of %s: %w", blobHash, err)
	}
	return stale, nil
}

// clearRelease forgets release intent for a blob that is wanted again (a new
// pin or taken placement) or whose bytes are gone.
func clearRelease(ctx context.Context, tx *sql.Tx, blobHash string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM blob_releases WHERE blob_hash = ?`, blobHash); err != nil {
		return fmt.Errorf("catalog: clearing the release of %s: %w", blobHash, err)
	}
	return nil
}
