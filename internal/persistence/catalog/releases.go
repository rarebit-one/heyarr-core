package catalog

import (
	"context"
	"database/sql"
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
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO blob_releases (blob_hash, cause, source_peer_id, released_at)
		SELECT ?, ?, ?, ?
		 WHERE NOT EXISTS (SELECT 1 FROM placement_pins WHERE blob_hash = ?)
		   AND NOT EXISTS (SELECT 1 FROM peer_placements WHERE blob_hash = ?)
		   AND NOT EXISTS (SELECT 1 FROM assets WHERE blob_hash = ?)
		   AND NOT EXISTS (SELECT 1 FROM scanned_files WHERE blob_hash = ?)
		ON CONFLICT (blob_hash) DO UPDATE SET
		   cause = excluded.cause, source_peer_id = excluded.source_peer_id,
		   released_at = excluded.released_at`,
		blobHash, cause, source, now, blobHash, blobHash, blobHash, blobHash); err != nil {
		return fmt.Errorf("catalog: recording the release of %s: %w", blobHash, err)
	}
	return nil
}

// clearRelease forgets release intent for a blob that is wanted again (a new
// pin or taken placement) or whose bytes are gone.
func clearRelease(ctx context.Context, tx *sql.Tx, blobHash string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM blob_releases WHERE blob_hash = ?`, blobHash); err != nil {
		return fmt.Errorf("catalog: clearing the release of %s: %w", blobHash, err)
	}
	return nil
}
