-- +goose NO TRANSACTION
-- +goose Up
-- Give desired_items an aspect, so a subtitle can be its own want (ADR-0085).
--
-- 00041 widened the table to the item scope. This widens it again, on a
-- different axis: which FACET of the target a want is about. Almost every want
-- is about the target's own content (aspect 'primary'); a subtitle is a caption
-- for the target's video (aspect 'subtitle', in a language), and ADR-0085 makes
-- it its OWN want rather than a fifth axis on acquisition_state — because a
-- subtitle is a different asset acquired by a different subsystem, so obtaining
-- it is different work from obtaining the video, and collapsing the two into one
-- want's state is exactly what ADR-0027 refuses.
--
-- # Why a rebuild and not an ALTER
--
-- SQLite can ADD a column but cannot add a table-level CHECK, and this change
-- needs one: aspect and language must agree (a subtitle names a language, a
-- primary want does not) and a subtitle must point at concrete video bytes (an
-- item or an edition, never the whole work). That is the same reason 00041 was a
-- rebuild, and it follows 00041's twelve-step shape exactly. The unique index
-- also gains aspect and language: without them a subtitle want and the primary
-- want over one target with one profile would collide, and the subtitle could
-- never be created.
--
-- # Why NO TRANSACTION (see 00041)
--
-- Five tables reference desired_items with ON DELETE CASCADE, so the rebuild
-- runs with foreign_keys OFF — untoggleable inside a transaction — on the single
-- writer connection, and the child FKs resolve by name after the rename.

PRAGMA foreign_keys = OFF;

CREATE TABLE desired_items_new (
    id TEXT PRIMARY KEY,

    scope TEXT NOT NULL DEFAULT 'work' CHECK (scope IN ('work', 'edition', 'item')),

    work_id    TEXT NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    edition_id TEXT REFERENCES editions (id) ON DELETE CASCADE,
    item_id    TEXT REFERENCES items (id) ON DELETE CASCADE,

    -- The facet of the target this want is about (ADR-0085). 'primary' is the
    -- content itself and the default every pre-existing row is; 'subtitle' is a
    -- caption for the target's video, in `language`.
    aspect TEXT NOT NULL DEFAULT 'primary' CHECK (aspect IN ('primary', 'subtitle')),

    -- The subtitle language as an ISO-639-1 code, set only for a subtitle want
    -- and empty otherwise. It is part of the want's identity (the index below),
    -- so the English and the German subtitle of one target are two wants.
    language TEXT NOT NULL DEFAULT '',

    quality_profile_id TEXT NOT NULL REFERENCES quality_profiles (id) ON DELETE RESTRICT,

    monitor INTEGER NOT NULL DEFAULT 1 CHECK (monitor IN (0, 1)),
    reason TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    CHECK (
        (scope = 'item' AND item_id IS NOT NULL AND edition_id IS NULL)
        OR (scope = 'edition' AND edition_id IS NOT NULL AND item_id IS NULL)
        OR (scope = 'work' AND edition_id IS NULL AND item_id IS NULL)
    ),

    -- Aspect and language agree, and a subtitle captions concrete video bytes.
    -- A 'subtitle' want names a language and points at an item or an edition; a
    -- 'primary' want names no language. This is the domain's desired.Item.Validate
    -- rule, enforced again where the row lives.
    CHECK (
        (aspect = 'subtitle' AND language <> '' AND scope IN ('item', 'edition'))
        OR (aspect = 'primary' AND language = '')
    )
) STRICT;

-- Every existing row is a primary want with no language: they predate the aspect.
INSERT INTO desired_items_new
    (id, scope, work_id, edition_id, item_id, aspect, language,
     quality_profile_id, monitor, reason, created_at, updated_at)
SELECT id, scope, work_id, edition_id, item_id, 'primary', '',
       quality_profile_id, monitor, reason, created_at, updated_at
FROM desired_items;

DROP TABLE desired_items;
ALTER TABLE desired_items_new RENAME TO desired_items;

-- One want per (target, aspect, language, profile). aspect and language join the
-- key so a subtitle want does not collide with the primary want over the same
-- target and profile. language is NOT NULL so it needs no coalesce; the two
-- nullable target columns still do (NULL <> NULL in SQL).
CREATE UNIQUE INDEX desired_items_one_per_target_and_profile
    ON desired_items (scope, work_id, coalesce(edition_id, ''), coalesce(item_id, ''),
        aspect, language, quality_profile_id);

CREATE INDEX desired_items_by_created ON desired_items (created_at, id);
CREATE INDEX desired_items_by_work ON desired_items (work_id);
CREATE INDEX desired_items_monitored ON desired_items (monitor) WHERE monitor = 1;

PRAGMA foreign_keys = ON;

-- +goose NO TRANSACTION
-- +goose Down
-- Reverse the aspect widening: back to the 00041 shape. A subtitle want cannot
-- exist in the narrower shape, so it is dropped by the copy's WHERE — on a clean
-- rollback there are none.

PRAGMA foreign_keys = OFF;

CREATE TABLE desired_items_old (
    id TEXT PRIMARY KEY,
    scope TEXT NOT NULL DEFAULT 'work' CHECK (scope IN ('work', 'edition', 'item')),
    work_id    TEXT NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    edition_id TEXT REFERENCES editions (id) ON DELETE CASCADE,
    item_id    TEXT REFERENCES items (id) ON DELETE CASCADE,
    quality_profile_id TEXT NOT NULL REFERENCES quality_profiles (id) ON DELETE RESTRICT,
    monitor INTEGER NOT NULL DEFAULT 1 CHECK (monitor IN (0, 1)),
    reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (
        (scope = 'item' AND item_id IS NOT NULL AND edition_id IS NULL)
        OR (scope = 'edition' AND edition_id IS NOT NULL AND item_id IS NULL)
        OR (scope = 'work' AND edition_id IS NULL AND item_id IS NULL)
    )
) STRICT;

INSERT INTO desired_items_old
    (id, scope, work_id, edition_id, item_id, quality_profile_id, monitor, reason,
     created_at, updated_at)
SELECT id, scope, work_id, edition_id, item_id, quality_profile_id, monitor, reason,
       created_at, updated_at
FROM desired_items
WHERE aspect = 'primary';

DROP TABLE desired_items;
ALTER TABLE desired_items_old RENAME TO desired_items;

CREATE UNIQUE INDEX desired_items_one_per_target_and_profile
    ON desired_items (scope, work_id, coalesce(edition_id, ''), coalesce(item_id, ''),
        quality_profile_id);
CREATE INDEX desired_items_by_created ON desired_items (created_at, id);
CREATE INDEX desired_items_by_work ON desired_items (work_id);
CREATE INDEX desired_items_monitored ON desired_items (monitor) WHERE monitor = 1;

PRAGMA foreign_keys = ON;
