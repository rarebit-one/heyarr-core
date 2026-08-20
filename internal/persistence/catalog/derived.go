package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
)

// RecordDerived attaches a remux output to the same Edition as its source
// (§11, M2-10).
//
// # It is an Asset, not a cache entry
//
// A remux is "a usable local representation" of the same Edition, which is
// exactly what §11 calls an Asset. Recording it as one means replication
// (M4), integrity checking and garbage collection treat it like every other
// managed asset with no special case — behaviour that would otherwise have to
// be written four times, once per subsystem, and got wrong at least once.
//
// The consequence is deliberate and worth stating: a derived asset WILL be
// replicated to peers and WILL be reclaimed by ADR-0018's sweep when nothing
// references it. Both are right — a remux another site would also need is
// worth having there, and one nothing points at is worth reclaiming — but
// neither is free, and an operator seeing storage grow after a transcode
// should find this comment rather than a mystery.
//
// # Idempotent
//
// The job will be re-run (invariant 9). A second run of the same remux
// produces bytes the CAS deduplicates, and this converges on the same asset
// row rather than adding a second one.
func (c *Catalog) RecordDerived(
	ctx context.Context, sourceAssetID, blobHash string, size int64, container string, now time.Time,
) error {
	var pending []events.Event

	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		var editionID, libraryID sql.NullString
		var filename sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT edition_id, library_id, filename FROM assets WHERE id = ?`, sourceAssetID).
			Scan(&editionID, &libraryID, &filename); err != nil {
			return fmt.Errorf("catalog: the source asset for a remux is gone: %w", err)
		}

		stamp := now.Format(timestampFormat)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO blobs (hash, size, mime, chunked, first_seen_at)
			VALUES (?, ?, ?, 0, ?)
			ON CONFLICT (hash) DO NOTHING`,
			blobHash, size, mimeForContainer(container), stamp); err != nil {
			return err
		}

		// Keyed on (edition, blob, role) rather than a generated id, so a
		// re-run finds the row it wrote last time. Without that, every retried
		// remux would add an asset and the edition would accumulate identical
		// derivatives.
		var existing string
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM assets WHERE edition_id = ? AND blob_hash = ? AND role = ?`,
			editionID.String, blobHash, ffmpeg.DerivedRole).Scan(&existing)
		switch {
		case err == nil:
			_, err = tx.ExecContext(ctx, `UPDATE assets SET updated_at = ? WHERE id = ?`, stamp, existing)
			return err
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}

		assetID := uuid.Must(uuid.NewV7()).String()
		derivedName := deriveFilename(filename.String, container)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
				source_path, role, filename, mime, identification_source, created_at, updated_at)
			VALUES (?, ?, ?, 'managed', ?, NULL, ?, ?, ?, 'derived', ?, ?)`,
			assetID, editionID.String, nullableString(libraryID), blobHash,
			ffmpeg.DerivedRole, derivedName, mimeForContainer(container), stamp, stamp); err != nil {
			return err
		}

		var peerID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM peers WHERE is_self = 1`).
			Scan(&peerID); err != nil {
			return fmt.Errorf("catalog: resolving this peer for a derived replica: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, updated_at)
			VALUES (?, ?, 'present', ?, ?, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = 'present', bytes_present = excluded.bytes_present, updated_at = excluded.updated_at`,
			blobHash, peerID, size, stamp, stamp); err != nil {
			return err
		}

		e, err := c.events.EmitTx(ctx, tx, events.TypeAssetCreated, "asset", assetID,
			map[string]any{
				"asset_id": assetID, "edition_id": editionID.String,
				"blob_hash": blobHash, "role": ffmpeg.DerivedRole,
				"derived_from": sourceAssetID, "container": container,
			})
		if err != nil {
			return err
		}
		pending = append(pending, e)
		return nil
	})
	if err != nil {
		return err
	}
	c.events.Publish(pending...)
	return nil
}

func nullableString(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

// mimeForContainer names the output. It is a small closed set because
// Milestone 2 remuxes into exactly two containers.
func mimeForContainer(container string) string {
	switch container {
	case "mp4":
		return "video/mp4"
	case "mkv":
		return "video/x-matroska"
	default:
		return ""
	}
}

// deriveFilename names the output after its source, so a person looking at the
// edition can tell which file a derivative came from.
func deriveFilename(source, container string) string {
	if source == "" {
		return "derived." + container
	}
	base := source
	if i := lastDot(source); i > 0 {
		base = source[:i]
	}
	return base + " [" + container + "]." + container
}

func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}
