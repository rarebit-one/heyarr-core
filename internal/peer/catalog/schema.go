package catalog

// schemaSQL is the snapshot database's entire schema.
//
// # Why this is not a goose migration
//
// Because it is not the control database, and the whole point of this package
// is that the two are different files with different lifecycles (§52,
// Invariant 5). Migration 00024 adds peer_snapshots to the CONTROLLER — the
// control plane's index of which snapshot each peer holds — and that table
// belongs in the control plane's migration lineage because §79 lists it among
// the control tables.
//
// This schema does not, for a reason stronger than tidiness: a snapshot is
// disposable. It is a materialised view of somebody else's catalogue, and the
// correct response to a snapshot store that cannot be opened at the expected
// schema is to delete it and rebuild from the controller — which is exactly
// what [Open] does. Migrating it would be carefully preserving data that is
// definitionally reproducible, and would put a second migration lineage on a
// peer that ADR-0029 says runs no control plane at all.
//
// # STRICT, and foreign keys ON
//
// STRICT for the reason 00002_core.sql gives: SQLite's default affinity will
// store the string 'banana' in an INTEGER column, and a snapshot that silently
// accepts nonsense is worse than one that rejects it. Foreign keys because a
// snapshot with a dangling edition_id is worthless to M7 in precisely the
// situation M7 exists for — and the schema is the half of that check that also
// holds when a row arrives through a repair by hand.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS snapshot_meta (
    -- One row, forever. A snapshot store holds THE snapshot; a second row
    -- would make "which one is current?" a question answered by ORDER BY.
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    controller_id  TEXT NOT NULL,
    version        INTEGER NOT NULL CHECK (version > 0),
    generated_at   TEXT NOT NULL,
    kind           TEXT NOT NULL CHECK (kind IN ('full', 'incremental')),
    watermark      TEXT NOT NULL,
    applied_at     TEXT NOT NULL,
    content_digest TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS snapshot_libraries (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    content_type TEXT NOT NULL,
    enabled      INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    created_at   TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS snapshot_library_roots (
    id          TEXT PRIMARY KEY,
    library_id  TEXT NOT NULL REFERENCES snapshot_libraries (id) ON DELETE CASCADE,
    path        TEXT NOT NULL,
    ingest_mode TEXT NOT NULL,
    enabled     INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    created_at  TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS snapshot_works (
    id           TEXT PRIMARY KEY,
    content_type TEXT NOT NULL,
    work_key     TEXT NOT NULL,
    title        TEXT NOT NULL,
    sort_title   TEXT NOT NULL,
    year         INTEGER,
    attributes   TEXT NOT NULL CHECK (json_valid(attributes)),
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS snapshot_works_by_type_and_title
    ON snapshot_works (content_type, sort_title);

CREATE TABLE IF NOT EXISTS snapshot_editions (
    id           TEXT PRIMARY KEY,
    work_id      TEXT NOT NULL REFERENCES snapshot_works (id) ON DELETE CASCADE,
    label        TEXT NOT NULL,
    edition_type TEXT NOT NULL,
    language     TEXT,
    attributes   TEXT NOT NULL CHECK (json_valid(attributes)),
    created_at   TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS snapshot_editions_by_work ON snapshot_editions (work_id);

CREATE TABLE IF NOT EXISTS snapshot_blobs (
    -- Byte identity, and nothing else is (Invariant 1, ADR-0005). The snapshot
    -- carries the identity and the shape; the bytes themselves are the CAS's,
    -- which this peer already holds in full (§80).
    hash          TEXT PRIMARY KEY,
    size          INTEGER NOT NULL CHECK (size >= 0),
    mime          TEXT,
    chunked       INTEGER NOT NULL CHECK (chunked IN (0, 1)),
    first_seen_at TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS snapshot_assets (
    id           TEXT PRIMARY KEY,
    edition_id   TEXT NOT NULL REFERENCES snapshot_editions (id) ON DELETE CASCADE,
    library_id   TEXT REFERENCES snapshot_libraries (id) ON DELETE SET NULL,
    source_class TEXT NOT NULL,
    -- ON DELETE RESTRICT would refuse a prune that removes a blob and its
    -- asset in the same refresh, which is an ordinary catalogue deletion. The
    -- store prunes children before parents, so the restriction buys nothing
    -- here that the ordering does not already give.
    blob_hash    TEXT REFERENCES snapshot_blobs (hash) ON DELETE SET NULL,
    source_path  TEXT,
    fingerprint  TEXT,
    role         TEXT NOT NULL,
    filename     TEXT,
    mime         TEXT,
    identification_source TEXT NOT NULL,
    missing_since TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS snapshot_assets_by_edition ON snapshot_assets (edition_id);
CREATE INDEX IF NOT EXISTS snapshot_assets_by_blob ON snapshot_assets (blob_hash)
    WHERE blob_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS snapshot_assets_by_library ON snapshot_assets (library_id);
`
