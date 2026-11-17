-- +goose NO TRANSACTION
-- +goose Up
-- Peer-pull of a non-swarm asset (ADR-0119 amendment, #833): the catalog
-- op-log holds `hold` ops, and a node records what it did with each one.
--
-- # The op kind
--
-- 00066 pinned the op column to delete/restore/acquire with a CHECK, and SQLite
-- cannot alter a CHECK in place, so the table is rebuilt: copy, drop, rename,
-- re-index. Unlike 00066 this runs with foreign_keys OFF, outside a
-- transaction (the house pattern, 00051): catalog_op_applications (00069)
-- references catalog_ops ON DELETE CASCADE, and with enforcement on, dropping
-- the old table would delete every recorded fan-out outcome with it. The new
-- table is renamed into the old name, so the reference resolves to it again.
--
-- # The outcomes
--
-- What this node did with each hold op, by op hash: a local side effect that
-- must happen once and survive a restart, like catalog_op_applications for
-- acquires. A table of its own rather than more outcomes there: a hold names a
-- blob and a library, not a want, and widening that CHECK would be a second
-- rebuild. `own` and `held` are final; the rest are re-evaluated every pass.
-- blob_hash is the op's digest, denormalised so get_replica_status can say what
-- became of a blob without re-evaluating the log.

PRAGMA foreign_keys = OFF;

CREATE TABLE catalog_ops_new (
    op_hash      TEXT PRIMARY KEY,
    content_type TEXT NOT NULL,
    work_key     TEXT NOT NULL,
    op           TEXT NOT NULL               -- 'delete' | 'restore' | 'acquire' | 'hold'
                 CHECK (op IN ('delete', 'restore', 'acquire', 'hold')),
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

CREATE TABLE catalog_hold_applications (
    op_hash    TEXT PRIMARY KEY REFERENCES catalog_ops (op_hash) ON DELETE CASCADE,
    blob_hash  TEXT NOT NULL
               CHECK (blob_hash GLOB 'blake3:[0-9a-f]*' AND length(blob_hash) = 71),
    -- The library the hold landed in, or was chosen for; NULL before one is.
    library_id TEXT REFERENCES libraries (id) ON DELETE SET NULL,
    outcome    TEXT NOT NULL
               CHECK (outcome IN ('own', 'held', 'pulling', 'no_library', 'too_large',
                                  'no_site_source', 'tombstoned')),
    detail     TEXT NOT NULL DEFAULT '',
    applied_at TEXT NOT NULL                 -- RFC3339 UTC (ADR-0017)
) STRICT;
CREATE INDEX catalog_hold_applications_by_blob ON catalog_hold_applications (blob_hash);

PRAGMA foreign_keys = ON;

-- +goose NO TRANSACTION
-- +goose Down
-- Back to the 00066 kinds. A hold op would fail the narrower CHECK, so it is
-- dropped: holds are re-derivable from the sibling that signed them, and a
-- downgraded node cannot act on them.
--
-- REFUSED, before anything changes, when any other op cites a hold in its prev:
-- dropping the hold would leave that op with a missing past, Evaluate would
-- reject it as bad_prev, and a tombstone or an acquire in force would lapse.
-- The refusal is a CHECK on a one-row guard table, as in 00066. It runs first
-- and with foreign keys still on, so a refused rollback leaves every row and
-- the connection's pragmas exactly as they were.
DROP TABLE IF EXISTS catalog_ops_down_guard;
CREATE TEMP TABLE catalog_ops_down_guard (
    cited_holds INTEGER NOT NULL
        CHECK (cited_holds = 0)  -- another op cites a hold op; see above
);
INSERT INTO catalog_ops_down_guard (cited_holds)
SELECT count(*)
FROM catalog_ops h
WHERE h.op = 'hold'
  AND EXISTS (
      SELECT 1 FROM catalog_ops o, json_each(o.prev) p
      WHERE o.op <> 'hold' AND p.value = h.op_hash
  );
DROP TABLE catalog_ops_down_guard;

DROP TABLE catalog_hold_applications;

PRAGMA foreign_keys = OFF;

CREATE TABLE catalog_ops_old (
    op_hash      TEXT PRIMARY KEY,
    content_type TEXT NOT NULL,
    work_key     TEXT NOT NULL,
    op           TEXT NOT NULL
                 CHECK (op IN ('delete', 'restore', 'acquire')),
    signer       TEXT NOT NULL,
    prev         TEXT NOT NULL,
    iat          TEXT NOT NULL,
    token        TEXT NOT NULL,
    received_at  TEXT NOT NULL
) STRICT;
INSERT INTO catalog_ops_old
    (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
SELECT op_hash, content_type, work_key, op, signer, prev, iat, token, received_at
FROM catalog_ops WHERE op <> 'hold';
DROP INDEX catalog_ops_by_target;
DROP TABLE catalog_ops;
ALTER TABLE catalog_ops_old RENAME TO catalog_ops;
CREATE INDEX catalog_ops_by_target ON catalog_ops (content_type, work_key, iat);

PRAGMA foreign_keys = ON;
