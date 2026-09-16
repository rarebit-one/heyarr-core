-- +goose NO TRANSACTION
-- +goose Up
-- Add 'grab_failed' to the reasons a release can be blocked for (§64, M3-13).
--
-- 00018 gave blocked_releases three reasons: 'verification_failed' (bad bytes),
-- 'ingest_failed' (good bytes, a local problem), and 'manual'. A fourth is
-- needed now: a release the download client could never START fetching — the
-- tracker refused the infohash ("info hash is not authorized with this
-- tracker"), or the magnet resolved to nothing. That is not bad bytes and it is
-- not a local problem; it is a dead SOURCE, and the want must try a different
-- release rather than re-select this one every search and stall on it forever
-- (the exact loop this table exists to break — see 00018's header).
--
-- Recorded as its own reason, not folded into 'verification_failed', for the
-- same reason 00018 kept 'ingest_failed' separate: a future policy may want to
-- treat a tracker rejection as possibly-transient and expire the block, where a
-- hash mismatch is permanent. One word for both would bake that decision in now.
--
-- # Why a rebuild and not an ALTER
--
-- SQLite cannot alter a table-level CHECK, and the reason column carries one
-- (00018). Extending the allowed set is therefore the twelve-step rebuild, the
-- same shape 00046 uses.
--
-- # Why NO TRANSACTION
--
-- The rebuild runs with foreign_keys OFF — untoggleable inside a transaction.
-- blocked_releases references desired_items with ON DELETE CASCADE and nothing
-- references it, so the toggle is belt-and-braces, matching the house pattern.

PRAGMA foreign_keys = OFF;

CREATE TABLE blocked_releases_new (
    id TEXT PRIMARY KEY,

    desired_item_id TEXT NOT NULL
        REFERENCES desired_items (id) ON DELETE CASCADE,

    provider     TEXT NOT NULL,
    candidate_id TEXT NOT NULL,

    title TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',

    reason TEXT NOT NULL DEFAULT 'verification_failed'
        CHECK (reason IN ('verification_failed', 'ingest_failed', 'grab_failed', 'manual')),

    blocked_at TEXT NOT NULL,

    UNIQUE (desired_item_id, provider, candidate_id)
) STRICT;

INSERT INTO blocked_releases_new
    (id, desired_item_id, provider, candidate_id, title, detail, reason, blocked_at)
SELECT id, desired_item_id, provider, candidate_id, title, detail, reason, blocked_at
FROM blocked_releases;

DROP TABLE blocked_releases;
ALTER TABLE blocked_releases_new RENAME TO blocked_releases;

CREATE INDEX blocked_releases_by_want ON blocked_releases (desired_item_id);

PRAGMA foreign_keys = ON;

-- +goose NO TRANSACTION
-- +goose Down
-- Back to the 00018 shape: three reasons. A row blocked as 'grab_failed' cannot
-- exist in the narrower CHECK, so the copy's WHERE drops it — on a clean
-- rollback there are none.

PRAGMA foreign_keys = OFF;

CREATE TABLE blocked_releases_old (
    id TEXT PRIMARY KEY,

    desired_item_id TEXT NOT NULL
        REFERENCES desired_items (id) ON DELETE CASCADE,

    provider     TEXT NOT NULL,
    candidate_id TEXT NOT NULL,

    title TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',

    reason TEXT NOT NULL DEFAULT 'verification_failed'
        CHECK (reason IN ('verification_failed', 'ingest_failed', 'manual')),

    blocked_at TEXT NOT NULL,

    UNIQUE (desired_item_id, provider, candidate_id)
) STRICT;

INSERT INTO blocked_releases_old
    (id, desired_item_id, provider, candidate_id, title, detail, reason, blocked_at)
SELECT id, desired_item_id, provider, candidate_id, title, detail, reason, blocked_at
FROM blocked_releases
WHERE reason <> 'grab_failed';

DROP TABLE blocked_releases;
ALTER TABLE blocked_releases_old RENAME TO blocked_releases;

CREATE INDEX blocked_releases_by_want ON blocked_releases (desired_item_id);

PRAGMA foreign_keys = ON;
