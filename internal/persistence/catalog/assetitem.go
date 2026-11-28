package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
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

// LinkFurtherEpisodes links an asset to the further episodes of the same season
// its file name spells, beyond the one item it is already linked to (#939,
// ADR-0086 amendment). An "S03E08E09" file linked to S03E08 is also linked to
// S03E09, so the E09 want is satisfied by the bytes already held rather than
// searched for forever.
//
// The asserted item stays the anchor. Nothing is linked unless the name agrees
// with it — same season, and the asserted episode among those the name spells —
// so a mislabelled file never spreads onto episodes it was not acquired for. A
// further episode is linked only to an item the work already ENUMERATED (matched
// by its item_key), never invented. Idempotent: a re-run links nothing new, so a
// retried ingest converges (invariant 9). Returns how many links were added.
func (c *Catalog) LinkFurtherEpisodes(ctx context.Context, assetID, itemID, fileName string) (int, error) {
	if assetID == "" || itemID == "" {
		return 0, nil
	}
	var added int
	err := c.db.InTx(ctx, func(tx *sql.Tx) error {
		var workID, itemKey string
		err := tx.QueryRowContext(ctx,
			`SELECT work_id, item_key FROM items WHERE id = ?`, itemID).Scan(&workID, &itemKey)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // the item is gone; there is nothing to anchor to
		}
		if err != nil {
			return fmt.Errorf("catalog: reading item %s: %w", itemID, err)
		}
		n, err := c.linkFurtherEpisodesTx(ctx, tx, assetID, workID, itemKey, fileName)
		added = n
		return err
	})
	return added, err
}

// furtherEpisodeKeys returns the item_keys of the episodes a file name spells
// beyond the anchor episode, when the name agrees with the anchor: the same
// season, with the anchor's episode among those spelled. Anything else — a
// single-episode name, a season pack, a name for another season or episode, an
// anchor that is not an SxxEyy key — yields nothing.
func furtherEpisodeKeys(anchorKey, fileName string) []string {
	anchor := identification.ParseReleaseSeasonEpisode(anchorKey)
	if !anchor.Determined || len(anchor.Episodes) != 1 {
		return nil
	}
	name := identification.ParseReleaseSeasonEpisode(path.Base(strings.ReplaceAll(fileName, "\\", "/")))
	if !name.Determined || name.Season != anchor.Season || len(name.Episodes) < 2 ||
		!name.ContainsEpisode(anchor.Season, anchor.Episodes[0]) {
		return nil
	}
	var keys []string
	seen := map[int]bool{anchor.Episodes[0]: true}
	for _, e := range name.Episodes {
		if seen[e] {
			continue
		}
		seen[e] = true
		keys = append(keys, fmt.Sprintf("S%02dE%02d", name.Season, e))
	}
	return keys
}

func (c *Catalog) linkFurtherEpisodesTx(
	ctx context.Context, tx *sql.Tx, assetID, workID, anchorKey, fileName string,
) (int, error) {
	keys := furtherEpisodeKeys(anchorKey, fileName)
	if len(keys) == 0 {
		return 0, nil
	}
	now := c.clock.Now().Format(timestampFormat)
	var added int
	for _, key := range keys {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO asset_items (asset_id, item_id, created_at)
			SELECT ?, id, ? FROM items WHERE work_id = ? AND upper(item_key) = ?
			ON CONFLICT (asset_id, item_id) DO NOTHING`,
			assetID, now, workID, key)
		if err != nil {
			return added, fmt.Errorf("catalog: linking asset %s to %s: %w", assetID, key, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return added, err
		}
		added += int(n)
	}
	return added, nil
}

// BackfillFurtherEpisodes runs LinkFurtherEpisodes over every primary asset
// already linked to an item (#939): the multi-episode files acquired before
// asset_items existed. Idempotent and cheap — it reads only item-linked assets —
// so it runs on every start and adds nothing after the first. Returns how many
// links it added.
func (c *Catalog) BackfillFurtherEpisodes(ctx context.Context) (int, error) {
	todo, err := c.multiEpisodeAssets(ctx)
	if err != nil {
		return 0, err
	}
	var added int
	for _, r := range todo {
		err := c.db.InTx(ctx, func(tx *sql.Tx) error {
			n, err := c.linkFurtherEpisodesTx(ctx, tx, r.assetID, r.workID, r.itemKey, r.fileName)
			added += n
			return err
		})
		if err != nil {
			return added, err
		}
	}
	return added, nil
}

type itemLinkedAsset struct{ assetID, workID, itemKey, fileName string }

// multiEpisodeAssets reads the item-linked primary assets whose file name spells
// further episodes. It filters before any write: most assets are one-episode
// files, and a write transaction for each would hold the single writer for
// nothing.
func (c *Catalog) multiEpisodeAssets(ctx context.Context) ([]itemLinkedAsset, error) {
	rows, err := c.db.Reader().QueryContext(ctx, `
		SELECT a.id, i.work_id, i.item_key, coalesce(nullif(a.filename, ''), a.source_path, '')
		FROM assets a
		JOIN items i ON i.id = a.item_id
		WHERE a.role = 'primary' AND a.missing_since IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading item-linked assets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []itemLinkedAsset
	for rows.Next() {
		var r itemLinkedAsset
		if err := rows.Scan(&r.assetID, &r.workID, &r.itemKey, &r.fileName); err != nil {
			return nil, err
		}
		if len(furtherEpisodeKeys(r.itemKey, r.fileName)) > 0 {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}
