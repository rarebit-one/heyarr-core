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
--
-- REFUSED when a delete or restore cites an acquire op in its prev: dropping
-- the acquire would leave that tombstone op with a missing past, Evaluate
-- would reject it as bad_prev, and the next reconcile would lift a tombstone
-- that is in force. SQLite's RAISE is trigger-only, so the refusal is a CHECK
-- on a one-row guard table: the INSERT fails, and with it the migration, when
-- such a citation exists. Resolve by staying on 66, or by having the sibling
-- re-issue the delete with a prev that cites no acquire.
DROP TABLE IF EXISTS catalog_ops_down_guard;  -- a refused run leaves it behind on this connection
CREATE TEMP TABLE catalog_ops_down_guard (
    cited_acquires INTEGER NOT NULL
        CHECK (cited_acquires = 0)  -- a delete/restore cites an acquire op; see above
);
INSERT INTO catalog_ops_down_guard (cited_acquires)
SELECT count(*)
FROM catalog_ops a
WHERE a.op = 'acquire'
  AND EXISTS (
      SELECT 1 FROM catalog_ops d, json_each(d.prev) p
      WHERE d.op IN ('delete', 'restore') AND p.value = a.op_hash
  );
DROP TABLE catalog_ops_down_guard;
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
