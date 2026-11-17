-- +goose NO TRANSACTION
-- +goose Up
-- The dead-swarm fallback (ADR-0119 #800 amendment): the catalog op-log holds
-- `unsourced` ops, a sibling's report that it can resolve no source for an
-- acquire. The origin answers one with a hold.
--
-- Only the op column's CHECK changes, and SQLite cannot alter a CHECK in
-- place, so the table is rebuilt exactly as 00074 rebuilt it: foreign keys
-- OFF and outside a transaction, because catalog_op_applications (00069) and
-- catalog_hold_applications (00074) reference catalog_ops ON DELETE CASCADE
-- and would otherwise be emptied by the drop. No table is added: whether this
-- node already reported an acquire is read from the log itself.

PRAGMA foreign_keys = OFF;

CREATE TABLE catalog_ops_new (
    op_hash      TEXT PRIMARY KEY,
    content_type TEXT NOT NULL,
    work_key     TEXT NOT NULL,
    op           TEXT NOT NULL               -- 'delete' | 'restore' | 'acquire' | 'hold' | 'unsourced'
                 CHECK (op IN ('delete', 'restore', 'acquire', 'hold', 'unsourced')),
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

-- +goose NO TRANSACTION
-- +goose Down
-- Back to the 00074 kinds. An unsourced op would fail the narrower CHECK, so
-- it is dropped: a report is re-derivable from the sibling that signed it, and
-- a downgraded node cannot act on one.
--
-- REFUSED, before anything changes, when any other op cites an unsourced op in
-- its prev: dropping it would leave that op with a missing past, Evaluate would
-- reject it as bad_prev, and a tombstone, an acquire or a hold in force would
-- lapse. The guard is 00074's.
DROP TABLE IF EXISTS catalog_ops_down_guard;
CREATE TEMP TABLE catalog_ops_down_guard (
    cited_reports INTEGER NOT NULL
        CHECK (cited_reports = 0)  -- another op cites an unsourced op; see above
);
INSERT INTO catalog_ops_down_guard (cited_reports)
SELECT count(*)
FROM catalog_ops u
WHERE u.op = 'unsourced'
  AND EXISTS (
      SELECT 1 FROM catalog_ops o, json_each(o.prev) p
      WHERE o.op <> 'unsourced' AND p.value = u.op_hash
  );
DROP TABLE catalog_ops_down_guard;

PRAGMA foreign_keys = OFF;

CREATE TABLE catalog_ops_old (
    op_hash      TEXT PRIMARY KEY,
    content_type TEXT NOT NULL,
    work_key     TEXT NOT NULL,
    op           TEXT NOT NULL
                 CHECK (op IN ('delete', 'restore', 'acquire', 'hold')),
    signer       TEXT NOT NULL,
    prev         TEXT NOT NULL,
    iat          TEXT NOT NULL,
    token        TEXT NOT NULL,
    received_at  TEXT NOT NULL
) STRICT;
INSERT INTO catalog_ops_old
    (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
SELECT op_hash, content_type, work_key, op, signer, prev, iat, token, received_at
FROM catalog_ops WHERE op <> 'unsourced';
DROP INDEX catalog_ops_by_target;
DROP TABLE catalog_ops;
ALTER TABLE catalog_ops_old RENAME TO catalog_ops;
CREATE INDEX catalog_ops_by_target ON catalog_ops (content_type, work_key, iat);

PRAGMA foreign_keys = ON;
