-- +goose Up
-- A node-local repair hold is distinct from a bad release and survives restarts.
CREATE TABLE acquisition_repair_holds (
    desired_item_id TEXT PRIMARY KEY REFERENCES desired_items(id) ON DELETE CASCADE,
    detail TEXT NOT NULL,
    created_at TEXT NOT NULL
) STRICT;

-- +goose Down
-- An older binary cannot respect active holds.
CREATE TEMP TABLE repair_hold_downgrade_guard (n INTEGER NOT NULL CHECK (n = 0));
INSERT INTO repair_hold_downgrade_guard SELECT COUNT(*) FROM acquisition_repair_holds;
DROP TABLE repair_hold_downgrade_guard;
DROP TABLE acquisition_repair_holds;
