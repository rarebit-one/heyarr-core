-- +goose Up
-- Let an asset record the Item it belongs to (ADR-0086).
--
-- Assets attach to an EDITION, and an edition can group many items — a season is
-- one edition holding every episode's video and every episode's subtitle. There
-- has been no way to say WHICH episode an asset is, so "does this episode hold a
-- subtitle" collapses to "does the season", and the video-to-subtitle binding
-- has rested entirely on a filename-stem convention (renderers.go). ADR-0085's
-- per-episode subtitle wants need the precise answer, so an asset gains an
-- explicit, nullable link to its Item.
--
-- Nullable, and NULL is the honest default: a library-scan asset was matched to a
-- work by its path and no source ever named an item for it, and a subtitle for a
-- film attaches to the film's edition with no item in play. The link is set by
-- the acquisition that KNOWS the item (an item-scoped want carries the item id),
-- so it is populated going forward and left NULL where nothing knows — the same
-- "absent means nobody could tell" stance the probe attributes take.
--
-- SET NULL on delete, not CASCADE: an asset outliving the byte-less Item row it
-- was linked to is a real state (the item metadata was pruned, the bytes remain),
-- and it must not take the asset with it. This is a plain ADD COLUMN — no CHECK
-- changes — so it needs no table rebuild.
ALTER TABLE assets ADD COLUMN item_id TEXT REFERENCES items (id) ON DELETE SET NULL;

CREATE INDEX assets_by_item ON assets (item_id) WHERE item_id IS NOT NULL;

-- +goose Down
DROP INDEX assets_by_item;
ALTER TABLE assets DROP COLUMN item_id;
