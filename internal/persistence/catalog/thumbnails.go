package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Thumbnail status values as stored in derived_thumbnails.status.
const (
	ThumbnailStatusReady       = "ready"
	ThumbnailStatusUndecodable = "undecodable"
)

// ThumbnailStatus returns the current derivation state for the given
// source blob + bucket + recipe. status is "" and thumbHash is "" when no row
// exists for this (source, bucket, recipe) combination.
func (c *Catalog) ThumbnailStatus(ctx context.Context, sourceHash string, bucket, recipe int) (status, thumbHash string, err error) {
	var th sql.NullString
	err = c.db.Reader().QueryRowContext(ctx, `
		SELECT status, thumb_hash
		FROM derived_thumbnails
		WHERE source_hash = ? AND bucket = ? AND recipe = ?`, sourceHash, bucket, recipe).Scan(&status, &th)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("catalog: reading thumbnail status for %s bucket %d recipe %d: %w", sourceHash, bucket, recipe, err)
	}
	if th.Valid {
		thumbHash = th.String
	}
	return status, thumbHash, nil
}

// UpsertThumbnailReady records that bucket is ready for sourceHash with
// thumbHash as the derived blob. It emits TypeThumbnailReady in the same
// transaction (invariant 7) and returns all pending events for the caller to
// Publish (the ready event plus any TypeThumbnailDeleted events for stale
// recipe rows that were lazily removed).
//
// Stale rows: any row with the same (source_hash, bucket) but a different
// recipe is deleted in the same transaction and its thumb_hash loses its only
// reference, making it collectable by the normal GC grace-window sweep.
func (c *Catalog) UpsertThumbnailReady(
	ctx context.Context, tx *sql.Tx,
	sourceHash string, bucket, recipe int, thumbHash string, now time.Time,
) ([]events.Event, error) {
	stamp := now.UTC().Format(timestampFormat)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO derived_thumbnails (source_hash, bucket, recipe, status, thumb_hash, created_at)
		VALUES (?, ?, ?, 'ready', ?, ?)
		ON CONFLICT (source_hash, bucket, recipe) DO UPDATE SET
			status     = 'ready',
			thumb_hash = excluded.thumb_hash,
			created_at = excluded.created_at`,
		sourceHash, bucket, recipe, thumbHash, stamp)
	if err != nil {
		return nil, fmt.Errorf("catalog: upserting ready thumbnail for %s bucket %d recipe %d: %w", sourceHash, bucket, recipe, err)
	}

	// Lazily delete any rows for the same (source_hash, bucket) that use a
	// different recipe. Their thumb_hash now has no derivation row holding it,
	// so the GC will reclaim it after the grace window.
	staleEvents, err := c.deleteStaleRecipeRows(ctx, tx, sourceHash, bucket, recipe)
	if err != nil {
		return nil, err
	}

	ev, err := c.events.EmitTx(ctx, tx, events.TypeThumbnailReady, "blob", sourceHash, map[string]any{
		"bucket":     bucket,
		"recipe":     recipe,
		"thumb_hash": thumbHash,
		"created_at": stamp,
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: emitting thumbnail ready event for %s bucket %d recipe %d: %w", sourceHash, bucket, recipe, err)
	}

	pending := make([]events.Event, 0, len(staleEvents)+1)
	pending = append(pending, staleEvents...)
	pending = append(pending, ev)
	return pending, nil
}

// UpsertThumbnailUndecodable records that the source blob at sourceHash cannot
// be thumbnailed for the given bucket and recipe. It emits
// TypeThumbnailUndecodable in the same transaction (invariant 7). This is a
// permanent outcome; no retry will be attempted.
func (c *Catalog) UpsertThumbnailUndecodable(
	ctx context.Context, tx *sql.Tx,
	sourceHash string, bucket, recipe int, now time.Time,
) (events.Event, error) {
	stamp := now.UTC().Format(timestampFormat)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO derived_thumbnails (source_hash, bucket, recipe, status, thumb_hash, created_at)
		VALUES (?, ?, ?, 'undecodable', NULL, ?)
		ON CONFLICT (source_hash, bucket, recipe) DO UPDATE SET
			status     = 'undecodable',
			thumb_hash = NULL,
			created_at = excluded.created_at`,
		sourceHash, bucket, recipe, stamp)
	if err != nil {
		return events.Event{}, fmt.Errorf("catalog: upserting undecodable thumbnail for %s bucket %d recipe %d: %w", sourceHash, bucket, recipe, err)
	}
	ev, err := c.events.EmitTx(ctx, tx, events.TypeThumbnailUndecodable, "blob", sourceHash, map[string]any{
		"bucket":     bucket,
		"recipe":     recipe,
		"created_at": stamp,
	})
	if err != nil {
		return events.Event{}, fmt.Errorf("catalog: emitting thumbnail undecodable event for %s bucket %d recipe %d: %w", sourceHash, bucket, recipe, err)
	}
	return ev, nil
}

// DeleteThumbnailsForSource removes all derived_thumbnails rows for the given
// source blob (called by Reclaim when a source blob is collected, ADR-0018).
// It emits TypeThumbnailDeleted per row, all within the same transaction.
// The returned events must be Publish-ed by the caller after the transaction
// commits.
func (c *Catalog) DeleteThumbnailsForSource(
	ctx context.Context, tx *sql.Tx, sourceHash string,
) ([]events.Event, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT bucket, recipe, status, thumb_hash
		FROM derived_thumbnails
		WHERE source_hash = ?`, sourceHash)
	if err != nil {
		return nil, fmt.Errorf("catalog: querying derived thumbnails for source %s: %w", sourceHash, err)
	}
	defer func() { _ = rows.Close() }()

	type row struct {
		bucket    int
		recipe    int
		status    string
		thumbHash sql.NullString
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.bucket, &r.recipe, &r.status, &r.thumbHash); err != nil {
			return nil, fmt.Errorf("catalog: reading derived thumbnails for source %s: %w", sourceHash, err)
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: reading derived thumbnails for source %s: %w", sourceHash, err)
	}
	_ = rows.Close()

	if len(found) == 0 {
		return nil, nil
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM derived_thumbnails WHERE source_hash = ?`, sourceHash)
	if err != nil {
		return nil, fmt.Errorf("catalog: deleting derived thumbnails for source %s: %w", sourceHash, err)
	}

	pending := make([]events.Event, 0, len(found))
	for _, r := range found {
		payload := map[string]any{
			"source_hash": sourceHash,
			"bucket":      r.bucket,
			"recipe":      r.recipe,
			"status":      r.status,
		}
		if r.thumbHash.Valid {
			payload["thumb_hash"] = r.thumbHash.String
		}
		ev, err := c.events.EmitTx(ctx, tx, events.TypeThumbnailDeleted, "blob", sourceHash, payload)
		if err != nil {
			return nil, fmt.Errorf("catalog: emitting thumbnail deleted event for source %s bucket %d: %w", sourceHash, r.bucket, err)
		}
		pending = append(pending, ev)
	}
	return pending, nil
}

// deleteStaleRecipeRows removes derived_thumbnails rows for (source_hash,
// bucket) that have a recipe different from the current one. Called inside
// UpsertThumbnailReady to lazily clean up superseded derivations.
func (c *Catalog) deleteStaleRecipeRows(
	ctx context.Context, tx *sql.Tx,
	sourceHash string, bucket, currentRecipe int,
) ([]events.Event, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT recipe, status, thumb_hash
		FROM derived_thumbnails
		WHERE source_hash = ? AND bucket = ? AND recipe != ?`,
		sourceHash, bucket, currentRecipe)
	if err != nil {
		return nil, fmt.Errorf("catalog: querying stale recipe rows for %s bucket %d: %w", sourceHash, bucket, err)
	}
	defer func() { _ = rows.Close() }()

	type row struct {
		recipe    int
		status    string
		thumbHash sql.NullString
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.recipe, &r.status, &r.thumbHash); err != nil {
			return nil, fmt.Errorf("catalog: reading stale recipe rows for %s bucket %d: %w", sourceHash, bucket, err)
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: reading stale recipe rows for %s bucket %d: %w", sourceHash, bucket, err)
	}
	_ = rows.Close()

	if len(found) == 0 {
		return nil, nil
	}

	_, err = tx.ExecContext(ctx, `
		DELETE FROM derived_thumbnails
		WHERE source_hash = ? AND bucket = ? AND recipe != ?`,
		sourceHash, bucket, currentRecipe)
	if err != nil {
		return nil, fmt.Errorf("catalog: deleting stale recipe rows for %s bucket %d: %w", sourceHash, bucket, err)
	}

	pending := make([]events.Event, 0, len(found))
	for _, r := range found {
		payload := map[string]any{
			"source_hash": sourceHash,
			"bucket":      bucket,
			"recipe":      r.recipe,
			"status":      r.status,
			"reason":      "recipe_superseded",
		}
		if r.thumbHash.Valid {
			payload["thumb_hash"] = r.thumbHash.String
		}
		ev, err := c.events.EmitTx(ctx, tx, events.TypeThumbnailDeleted, "blob", sourceHash, payload)
		if err != nil {
			return nil, fmt.Errorf("catalog: emitting stale recipe deleted event for %s bucket %d recipe %d: %w", sourceHash, bucket, r.recipe, err)
		}
		pending = append(pending, ev)
	}
	return pending, nil
}

// RecordThumbnailReady is the high-level wrapper for UpsertThumbnailReady: it
// opens its own transaction, emits events (invariant 7), and publishes them
// after the transaction commits. Callers should use this rather than calling
// UpsertThumbnailReady directly.
//
// The thumbnail is registered as a blob with this peer's present replica in
// the same transaction, the way RecordDerived registers a remux: without a
// blobs row the collector, integrity checks and replication never see it, and
// the blob route has nothing to serve (Codex on #744).
func (c *Catalog) RecordThumbnailReady(
	ctx context.Context, sourceHash string, bucket, recipe int, thumbHash string, size int64, now time.Time,
) error {
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		pending = nil
		stamp := now.Format(timestampFormat)
		// The source may have been reclaimed while the job decoded it; a ready row
		// for a reclaimed source would outlive it (Codex on #744). Record nothing:
		// the stored thumbnail has no reference and the collector takes it.
		var sourceKnown int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM blobs WHERE hash = ?`, sourceHash).
			Scan(&sourceKnown); err != nil {
			return fmt.Errorf("catalog: checking a thumbnail's source: %w", err)
		}
		if sourceKnown == 0 {
			return nil
		}
		adopted, err := adoptBlob(ctx, tx, thumbHash, size, stamp)
		if err != nil {
			return fmt.Errorf("catalog: registering thumbnail blob: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE blobs SET mime = 'image/jpeg' WHERE hash = ? AND mime IS NULL`, thumbHash); err != nil {
			return fmt.Errorf("catalog: typing thumbnail blob: %w", err)
		}
		if adopted {
			ev, err := c.events.EmitTx(ctx, tx, events.TypeBlobCreated, "blob", thumbHash, map[string]any{
				"size": size, "derived_from": sourceHash, "bucket": bucket,
			})
			if err != nil {
				return err
			}
			pending = append(pending, ev)
		}
		var peerID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM peers WHERE is_self = 1`).Scan(&peerID); err != nil {
			return fmt.Errorf("catalog: resolving this peer for a thumbnail replica: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, updated_at)
			VALUES (?, ?, 'present', ?, ?, ?)
			ON CONFLICT (blob_hash, peer_id) DO UPDATE SET
				state = 'present', bytes_present = excluded.bytes_present,
				verified_at = excluded.verified_at, updated_at = excluded.updated_at
			WHERE replicas.state <> 'present' OR replicas.bytes_present <> excluded.bytes_present`,
			thumbHash, peerID, size, stamp, stamp)
		if err != nil {
			return fmt.Errorf("catalog: recording thumbnail replica: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("catalog: recording thumbnail replica: %w", err)
		} else if n == 1 {
			ev, err := c.events.EmitTx(ctx, tx, events.TypeReplicaPresent, "blob", thumbHash, map[string]any{
				"peer_id": peerID, "bytes_present": size,
			})
			if err != nil {
				return err
			}
			pending = append(pending, ev)
		}
		evs, err := c.UpsertThumbnailReady(ctx, tx, sourceHash, bucket, recipe, thumbHash, now)
		if err != nil {
			return err
		}
		pending = append(pending, evs...)
		return nil
	})
	if err != nil {
		return err
	}
	c.events.Publish(pending...)
	return nil
}

// RecordThumbnailUndecodable is the high-level wrapper for
// UpsertThumbnailUndecodable: it opens its own transaction, emits the event
// (invariant 7), and publishes it after the transaction commits.
func (c *Catalog) RecordThumbnailUndecodable(ctx context.Context, sourceHash string, bucket, recipe int, now time.Time) error {
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		ev, err := c.UpsertThumbnailUndecodable(ctx, tx, sourceHash, bucket, recipe, now)
		if err != nil {
			return err
		}
		pending = append(pending, ev)
		return nil
	})
	if err != nil {
		return err
	}
	c.events.Publish(pending...)
	return nil
}
