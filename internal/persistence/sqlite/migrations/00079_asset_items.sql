-- +goose Up
-- A multi-episode file belongs to every episode it holds (#939, ADR-0086
-- amendment). assets.item_id stays the ONE item the acquisition asserted;
-- this table records the further items of the same season the asset also
-- contains — the E09 of an "S03E08E09" file — so the E09 want is satisfied by
-- the bytes already held rather than searched for forever.
--
-- CASCADE on both sides: a row is a label joining an asset to an item, and it
-- means nothing once either is gone. (assets.item_id is SET NULL instead, because
-- there the label lives ON the asset row; here the row IS the label.)
CREATE TABLE asset_items (
    asset_id   TEXT NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
    item_id    TEXT NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (asset_id, item_id)
) STRICT;

-- Item-scoped satisfaction reads it by item.
CREATE INDEX asset_items_by_item ON asset_items (item_id);

-- +goose Down
DROP INDEX asset_items_by_item;
DROP TABLE asset_items;
