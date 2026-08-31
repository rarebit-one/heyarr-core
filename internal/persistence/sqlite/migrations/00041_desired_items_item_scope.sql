-- +goose NO TRANSACTION
-- +goose Up
-- Widen desired_items to the item scope (§55, M12, ADR-0056).
--
-- 00013 created desired_items with `scope IN ('work', 'edition')` and no way to
-- point at an episode, and said so plainly: "there is no 'episode' scope, and
-- that is a limitation rather than an omission ... when a metadata provider
-- lands, 'episode' is an addition here rather than a retrofit." M12's feed
-- adapter is that provider, and this is that addition: a third scope, 'item',
-- and an item_id that points at the byte-less Item row 00040 created.
--
-- # Why this is a table rebuild and not an ALTER
--
-- SQLite can ADD a column but cannot alter a CHECK constraint, and both of
-- 00013's live here: the column CHECK `scope IN ('work', 'edition')` and the
-- table CHECK that the scope and its target agree. Admitting a third scope means
-- rewriting both, which is the standard twelve-step rebuild — create the new
-- shape, copy, drop, rename, re-index — and it is the FIRST in this repo, so it
-- is spelled out.
--
-- # Why NO TRANSACTION, and why that is safe here
--
-- Five tables reference desired_items with ON DELETE CASCADE (acquisition_state,
-- release_candidates, blocked_releases, acquisitions, search_schedule). Dropping
-- the old table with foreign keys ON would fire those cascades and delete every
-- acquisition, candidate and schedule row in the process. The rebuild therefore
-- runs with foreign_keys OFF — which cannot be toggled inside a transaction — so
-- the migration declares NO TRANSACTION and toggles it itself. It runs on the
-- single writer connection (pool size 1), so the pragma holds for every
-- statement between the two toggles. The child FKs resolve BY NAME, so after the
-- rename they point at the rebuilt table with their rows intact and their ids
-- unchanged.

PRAGMA foreign_keys = OFF;

CREATE TABLE desired_items_new (
    id TEXT PRIMARY KEY,

    -- The third value, 'item', is the addition. An item-scoped want points at
    -- one byte-less Item (00040) — a single episode a source emitted.
    scope TEXT NOT NULL DEFAULT 'work' CHECK (scope IN ('work', 'edition', 'item')),

    work_id    TEXT NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    edition_id TEXT REFERENCES editions (id) ON DELETE CASCADE,

    -- Set only at item scope. CASCADE: a want for an item that no longer exists
    -- is a dangling reference, the same stance work_id and edition_id take.
    item_id    TEXT REFERENCES items (id) ON DELETE CASCADE,

    quality_profile_id TEXT NOT NULL REFERENCES quality_profiles (id) ON DELETE RESTRICT,

    monitor INTEGER NOT NULL DEFAULT 1 CHECK (monitor IN (0, 1)),
    reason TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    -- The scope and its target agree, and the database enforces it. Each scope
    -- names its own target and refuses the other two — an item-scoped row with
    -- an edition_id, or a work-scoped row with an item_id, is exactly the field
    -- something later reads without checking the scope.
    CHECK (
        (scope = 'item' AND item_id IS NOT NULL AND edition_id IS NULL)
        OR (scope = 'edition' AND edition_id IS NOT NULL AND item_id IS NULL)
        OR (scope = 'work' AND edition_id IS NULL AND item_id IS NULL)
    )
) STRICT;

-- item_id is NULL for every existing row: they predate the scope, and none of
-- them is item-scoped.
INSERT INTO desired_items_new
    (id, scope, work_id, edition_id, item_id, quality_profile_id, monitor, reason,
     created_at, updated_at)
SELECT id, scope, work_id, edition_id, NULL, quality_profile_id, monitor, reason,
       created_at, updated_at
FROM desired_items;

DROP TABLE desired_items;
ALTER TABLE desired_items_new RENAME TO desired_items;

-- One want per (target, profile). coalesce() over BOTH nullable target columns,
-- because NULL <> NULL in SQL: without it two work-scoped duplicates, or two
-- item-scoped duplicates, would both satisfy the constraint and the duplicate
-- this index exists to prevent would sail through (00013's own warning).
CREATE UNIQUE INDEX desired_items_one_per_target_and_profile
    ON desired_items (scope, work_id, coalesce(edition_id, ''), coalesce(item_id, ''),
        quality_profile_id);

CREATE INDEX desired_items_by_created ON desired_items (created_at, id);
CREATE INDEX desired_items_by_work ON desired_items (work_id);
CREATE INDEX desired_items_monitored ON desired_items (monitor) WHERE monitor = 1;

PRAGMA foreign_keys = ON;

-- +goose NO TRANSACTION
-- +goose Down
-- Reverse the widening: back to the two-scope table 00013 created. Item-scoped
-- rows cannot exist in the narrower shape, so they are dropped by the copy's
-- WHERE — on a clean rollback there are none.

PRAGMA foreign_keys = OFF;

CREATE TABLE desired_items_old (
    id TEXT PRIMARY KEY,
    scope TEXT NOT NULL DEFAULT 'work' CHECK (scope IN ('work', 'edition')),
    work_id    TEXT NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    edition_id TEXT REFERENCES editions (id) ON DELETE CASCADE,
    quality_profile_id TEXT NOT NULL REFERENCES quality_profiles (id) ON DELETE RESTRICT,
    monitor INTEGER NOT NULL DEFAULT 1 CHECK (monitor IN (0, 1)),
    reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (
        (scope = 'edition' AND edition_id IS NOT NULL)
        OR (scope = 'work' AND edition_id IS NULL)
    )
) STRICT;

INSERT INTO desired_items_old
    (id, scope, work_id, edition_id, quality_profile_id, monitor, reason,
     created_at, updated_at)
SELECT id, scope, work_id, edition_id, quality_profile_id, monitor, reason,
       created_at, updated_at
FROM desired_items
WHERE scope IN ('work', 'edition');

DROP TABLE desired_items;
ALTER TABLE desired_items_old RENAME TO desired_items;

CREATE UNIQUE INDEX desired_items_one_per_target_and_profile
    ON desired_items (scope, work_id, coalesce(edition_id, ''), quality_profile_id);
CREATE INDEX desired_items_by_created ON desired_items (created_at, id);
CREATE INDEX desired_items_by_work ON desired_items (work_id);
CREATE INDEX desired_items_monitored ON desired_items (monitor) WHERE monitor = 1;

PRAGMA foreign_keys = ON;
