-- +goose Up
-- The Milestone 1 slice of the controller data model (spec §79) lands in
-- M1-04. This migration exists so the migration machinery itself has something
-- real to apply, roll back, and be tested against.
CREATE TABLE schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

INSERT INTO schema_meta (key, value) VALUES ('initialised_by', 'heyarr');

-- +goose Down
DROP TABLE schema_meta;
