-- +goose Up
-- Drop schema_meta. 00001 created it only to give the migration machinery
-- something real to apply and roll back before the data model existed. The
-- only thing that ever read it was the rollback test, which now checks a real
-- table (blobs, from 00002). Goose's own goose_db_version records the
-- schema's version.
DROP TABLE schema_meta;

-- +goose Down
CREATE TABLE schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

INSERT INTO schema_meta (key, value) VALUES ('initialised_by', 'heyarr');
