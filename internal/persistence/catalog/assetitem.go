package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// SetAssetItem links an asset to the Item it belongs to (ADR-0086).
//
// The item is a fact the acquisition asserts, not one the file reveals, so it is
// set here — after the ingest pipeline produced the asset — by the worker that
// held the item-scoped want. Idempotent: re-linking to the same item is a no-op,
// so a retried ingest converges (invariant 9). An empty itemID clears the link,
// which is what an acquisition with no item (a work- or edition-scoped grab)
// leaves.
func (c *Catalog) SetAssetItem(ctx context.Context, assetID, itemID string) error {
	if assetID == "" {
		return fmt.Errorf("catalog: linking an asset to an item needs an asset id")
	}
	var item any
	if itemID != "" {
		item = itemID
	}
	now := c.clock.Now().Format(timestampFormat)
	return c.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE assets SET item_id = ?, updated_at = ? WHERE id = ?`,
			item, now, assetID)
		if err != nil {
			return fmt.Errorf("catalog: linking asset %s to item %s: %w", assetID, itemID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("catalog: no asset %s to link to an item", assetID)
		}
		return nil
	})
}
