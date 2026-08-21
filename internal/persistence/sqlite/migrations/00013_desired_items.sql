-- +goose Up
-- Desired items: "this content should exist under these conditions" (§55, M3-02).
--
-- 00012 is quality profiles (M3-01). 00014 is the acquisition state machine,
-- 00015 providers, 00016 acquisitions, 00017 release candidates, with 00018
-- held — claimed up front because two M1 branches once took non-contiguous
-- numbers and broke two rollback tests.
--
-- # A want must survive having nothing
--
-- The point of this table is to hold rows about content with no Asset, no Blob
-- and no bytes anywhere. That is the requirement most easily lost, because
-- every fixture in the repository has assets: a schema that only works once
-- something exists passes every test and fails the first real use.
--
-- So the anchor is a WORK — the semantic entity (§11), which exists whether or
-- not any bytes do — and never an Asset, which is a file that exists by
-- definition. There is deliberately no blob_hash, no asset_id and no path here.
--
-- # Two wants over one work is the point, not a bug
--
-- §61 lists "one version per title" among the things Heyarr avoids. The
-- living-room 2160p copy and the phone-sized copy of one film are two wants,
-- so uniqueness is over (target, profile) and never over the target alone.
--
-- The unique index is expressed over a computed target key rather than over
-- (work_id, edition_id, quality_profile_id), because NULL is not equal to
-- itself in SQL: two work-scoped rows with edition_id NULL would BOTH satisfy
-- a naive unique constraint and the duplicate this exists to prevent would
-- sail through. That is the single most likely defect in this migration.

CREATE TABLE desired_items (
    id TEXT PRIMARY KEY,                    -- UUIDv7 (ADR-0017)

    -- How much of the work is wanted.
    --
    -- There is no 'episode' scope, and that is a limitation rather than an
    -- omission. §11 makes the WORK the series and an edition the season, so an
    -- episode is an Asset — a file that exists — and there is no entity
    -- anywhere for an episode nobody has yet. Knowing which episodes SHOULD
    -- exist is what a metadata provider is for, and that is deferred out of
    -- Milestone 3. When one lands, 'episode' is an addition here rather than a
    -- retrofit.
    scope TEXT NOT NULL DEFAULT 'work' CHECK (scope IN ('work', 'edition')),

    -- Always set, even at edition scope: an edition without its work makes
    -- "what do I want from this series" a join to find out, and lets an
    -- edition-scoped want outlive the work it belongs to.
    --
    -- CASCADE because a want for a work that no longer exists is not history
    -- worth keeping — it is a dangling reference every read path would have to
    -- special-case.
    work_id    TEXT NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    edition_id TEXT REFERENCES editions (id) ON DELETE CASCADE,

    -- The standard this want is measured against (§62).
    --
    -- RESTRICT, not CASCADE, and not SET NULL. Deleting the standard by which
    -- desire is measured while leaving the desire would make satisfaction
    -- unanswerable — §56 has nothing to evaluate against. The API turns this
    -- into a 409 naming the profile.
    quality_profile_id TEXT NOT NULL REFERENCES quality_profiles (id) ON DELETE RESTRICT,

    -- "Keep looking for something better", which is NOT the same as wanting
    -- (§60 keeps both words). An unmonitored want that is satisfied is
    -- finished, terminal profile or not: the operator said "get me this", not
    -- "keep improving this". Running the upgrade loop over unmonitored items
    -- is how *arr installations re-download libraries nobody asked them to
    -- touch.
    monitor INTEGER NOT NULL DEFAULT 1 CHECK (monitor IN (0, 1)),

    -- Free text an operator may attach. Never interpreted. It exists because a
    -- library accumulates wants and six months later nobody remembers why one
    -- is there.
    reason TEXT NOT NULL DEFAULT '',

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    -- The scope and the target agree, and the database is the one enforcing
    -- it. An edition id sitting unused on a work-scoped row is exactly the
    -- kind of field something later reads without checking the scope.
    CHECK (
        (scope = 'edition' AND edition_id IS NOT NULL)
        OR (scope = 'work' AND edition_id IS NULL)
    )
) STRICT;

-- One want per (target, profile).
--
-- coalesce() rather than the bare columns: NULL <> NULL in SQL, so a unique
-- index over (work_id, edition_id, quality_profile_id) would let every
-- work-scoped duplicate through. The scope is in the key because an id is only
-- unique within its own table, and a work and an edition could in principle
-- collide.
CREATE UNIQUE INDEX desired_items_one_per_target_and_profile
    ON desired_items (scope, work_id, coalesce(edition_id, ''), quality_profile_id);

-- Listed by keyset over (created_at, id): a want has no name to sort by, and
-- the interesting order is the order they were asked for. id is in the key
-- because created_at is not unique and a non-unique keyset boundary skips and
-- repeats rows exactly as OFFSET does.
CREATE INDEX desired_items_by_created ON desired_items (created_at, id);

-- Reconciliation (§57) walks wants by work, and the upgrade scan walks the
-- monitored ones. Both are per-row lookups today and both become table scans
-- the moment a library is real.
CREATE INDEX desired_items_by_work ON desired_items (work_id);
CREATE INDEX desired_items_monitored ON desired_items (monitor) WHERE monitor = 1;

-- +goose Down
DROP INDEX desired_items_monitored;
DROP INDEX desired_items_by_work;
DROP INDEX desired_items_by_created;
DROP INDEX desired_items_one_per_target_and_profile;
DROP TABLE desired_items;
