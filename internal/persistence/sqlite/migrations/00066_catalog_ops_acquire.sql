-- +goose NO TRANSACTION
-- +goose Up
-- Let the catalog op-log hold `acquire` ops (ADR-0119). 00043 pinned the op
-- column to the two tombstone kinds with a CHECK, and SQLite cannot alter a
-- CHECK in place, so the table is rebuilt the way 00051 rebuilt
-- blocked_releases: copy, drop, rename, re-index. catalog_ops has no foreign
-- keys in either direction, so nothing dangles across the swap; foreign_keys is
-- toggled all the same, for the same reason as 00046 (an untoggleable PRAGMA
-- inside a transaction is a silent no-op, and NO TRANSACTION keeps it honest).
PRAGMA foreign_keys = OFF;
CREATE TABLE catalog_ops_new (
    op_hash      TEXT PRIMARY KEY,
    content_type TEXT NOT NULL,
    work_key     TEXT NOT NULL,
    op           TEXT NOT NULL               -- 'delete' | 'restore' | 'acquire'
                 CHECK (op IN ('delete', 'restore', 'acquire')),
    signer       TEXT NOT NULL,
    prev         TEXT NOT NULL,
    iat          TEXT NOT NULL,
    token        TEXT NOT NULL,
    received_at  TEXT NOT NULL
) STRICT;
INSERT INTO catalog_ops_new
    (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
SELECT op_hash, content_type, work_key, op, signer, prev, iat, token, received_at
FROM catalog_ops;
DROP INDEX catalog_ops_by_target;
DROP TABLE catalog_ops;
ALTER TABLE catalog_ops_new RENAME TO catalog_ops;
CREATE INDEX catalog_ops_by_target ON catalog_ops (content_type, work_key, iat);
PRAGMA foreign_keys = ON;

-- +goose Down
-- Back to the two tombstone kinds. An acquire op already in the log would fail
-- the narrower CHECK, so it is dropped on the way down: the ops are
-- re-derivable from the sibling that still holds them, and a downgraded node
-- cannot act on them anyway.
PRAGMA foreign_keys = OFF;
CREATE TABLE catalog_ops_old (
    op_hash      TEXT PRIMARY KEY,
    content_type TEXT NOT NULL,
    work_key     TEXT NOT NULL,
    op           TEXT NOT NULL
                 CHECK (op IN ('delete', 'restore')),
    signer       TEXT NOT NULL,
    prev         TEXT NOT NULL,
    iat          TEXT NOT NULL,
    token        TEXT NOT NULL,
    received_at  TEXT NOT NULL
) STRICT;
INSERT INTO catalog_ops_old
    (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
SELECT op_hash, content_type, work_key, op, signer, prev, iat, token, received_at
FROM catalog_ops WHERE op IN ('delete', 'restore');
DROP INDEX catalog_ops_by_target;
DROP TABLE catalog_ops;
ALTER TABLE catalog_ops_old RENAME TO catalog_ops;
CREATE INDEX catalog_ops_by_target ON catalog_ops (content_type, work_key, iat);
PRAGMA foreign_keys = ON;
