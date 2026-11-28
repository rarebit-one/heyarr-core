package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// RecordMusicMetadataMatch records a text-tag match to exact content bytes.
// This is not acoustic verification. Reconciliation and playback must use the
// evidence only while the current asset still points at this digest.
func (c *Catalog) RecordMusicMetadataMatch(ctx context.Context, workID, assetID, hash string) error {
	var pending []events.Event
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		var previous sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT json_extract(w.attributes,'$.metadata_matched_blob')
    FROM works w JOIN editions e ON e.work_id=w.id JOIN assets a ON a.edition_id=e.id
    WHERE w.id=? AND w.work_key LIKE 'music-recording:%' AND a.id=? AND a.blob_hash=? AND a.role='primary'`, workID, assetID, hash).Scan(&previous)
		if err != nil {
			return fmt.Errorf("catalog: recording metadata match does not name current asset bytes: %w", err)
		}
		if previous.Valid && previous.String == hash {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE works SET attributes=json_set(attributes,'$.metadata_matched_blob',?,'$.match_method','embedded_tags'), updated_at=? WHERE id=?`, hash, time.Now().UTC().Format(timestampFormat), workID)
		if err != nil {
			return err
		}
		e, err := c.events.EmitTx(ctx, tx, events.TypeWorkUpdated, "work", workID, map[string]any{"work_id": workID, "fields": []string{"metadata_matched_blob", "match_method"}, "match_method": "embedded_tags"})
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
