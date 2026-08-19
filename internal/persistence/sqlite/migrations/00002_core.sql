-- +goose Up
-- The Milestone 1 slice of the controller data model (spec §79).
--
-- Two things here exist before anything reads them, deliberately:
--
--   * peers/replicas are modelled multi-peer from the start even though there
--     is exactly one peer (ADR-0010), so Milestone 4 is a protocol addition
--     rather than a schema migration plus a rewrite of every read-path query.
--   * assets.source_class distinguishes managed / linked / vault (ADR-0020),
--     for the same reason: read routing in Milestone 4 must be able to express
--     "this exists on one peer by design" rather than "replication failed".
--
-- STRICT tables throughout: SQLite's default type affinity will happily store
-- the string 'banana' in an INTEGER column, and a catalog that silently accepts
-- nonsense is worse than one that rejects it.

-- Peers -----------------------------------------------------------------------
CREATE TABLE peers (
    id          TEXT PRIMARY KEY,           -- UUIDv7 (ADR-0017)
    name        TEXT NOT NULL UNIQUE,
    site        TEXT NOT NULL DEFAULT '',   -- failure domain (§35)
    mode        TEXT NOT NULL DEFAULT 'full'
                CHECK (mode IN ('full', 'partial', 'cache', 'archive', 'compute')),
    public_key  BLOB,                       -- Ed25519; reserved for M4 (ADR-0012)
    endpoint    TEXT,
    is_self     INTEGER NOT NULL DEFAULT 0 CHECK (is_self IN (0, 1)),
    created_at  TEXT NOT NULL
) STRICT;

-- Exactly one peer may be this node. Two rows claiming to be self is
-- unrecoverable once replication has run, so the database refuses it.
CREATE UNIQUE INDEX peers_only_one_self ON peers (is_self) WHERE is_self = 1;

-- Libraries -------------------------------------------------------------------
CREATE TABLE libraries (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    content_type TEXT NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at   TEXT NOT NULL
) STRICT;

CREATE TABLE library_roots (
    id           TEXT PRIMARY KEY,
    library_id   TEXT NOT NULL REFERENCES libraries (id) ON DELETE CASCADE,
    path         TEXT NOT NULL,
    -- How ingest materialises bytes into the CAS (ADR-0014). 'link' means the
    -- files are catalogued where they are and never copied (ADR-0020).
    ingest_mode  TEXT NOT NULL DEFAULT 'reflink'
                 CHECK (ingest_mode IN ('reflink', 'hardlink', 'copy', 'link')),
    enabled      INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at   TEXT NOT NULL,
    UNIQUE (library_id, path)
) STRICT;

-- Semantic content spine (§11) ------------------------------------------------
CREATE TABLE works (
    id           TEXT PRIMARY KEY,
    content_type TEXT NOT NULL,
    -- Normalised identity used for get-or-create, so a rescan converges on the
    -- same Work instead of multiplying it (M1-11).
    work_key     TEXT NOT NULL,
    title        TEXT NOT NULL,
    sort_title   TEXT NOT NULL,
    year         INTEGER,
    -- Per-content-type fields live here rather than as columns, so registering a
    -- fourth content type is not a migration. §12 lists thirteen; the failure
    -- mode this avoids is a works table with forty nullable columns.
    attributes   TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(attributes)),
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    UNIQUE (content_type, work_key)
) STRICT;

CREATE INDEX works_by_type_and_title ON works (content_type, sort_title);

CREATE TABLE editions (
    id           TEXT PRIMARY KEY,
    work_id      TEXT NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    label        TEXT NOT NULL DEFAULT '',
    edition_type TEXT NOT NULL DEFAULT '',
    language     TEXT,
    attributes   TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(attributes)),
    created_at   TEXT NOT NULL
) STRICT;

CREATE INDEX editions_by_work ON editions (work_id);

CREATE TABLE external_ids (
    id          TEXT PRIMARY KEY,
    entity_type TEXT NOT NULL CHECK (entity_type IN ('work', 'edition')),
    entity_id   TEXT NOT NULL,
    source      TEXT NOT NULL,
    value       TEXT NOT NULL,
    UNIQUE (source, value, entity_type)
) STRICT;

CREATE INDEX external_ids_by_entity ON external_ids (entity_type, entity_id);

-- Bytes (§13, §17) -------------------------------------------------------------
CREATE TABLE blobs (
    -- 'blake3:<64 lowercase hex>' — the canonical byte identity (ADR-0005).
    hash          TEXT PRIMARY KEY
                  CHECK (hash GLOB 'blake3:[0-9a-f]*' AND length(hash) = 71),
    size          INTEGER NOT NULL CHECK (size >= 0),
    mime          TEXT,
    chunked       INTEGER NOT NULL DEFAULT 0 CHECK (chunked IN (0, 1)),
    first_seen_at TEXT NOT NULL
) STRICT;

CREATE TABLE assets (
    id          TEXT PRIMARY KEY,
    edition_id  TEXT NOT NULL REFERENCES editions (id) ON DELETE CASCADE,
    library_id  TEXT REFERENCES libraries (id) ON DELETE SET NULL,

    -- ADR-0020. A linked asset has NO blob: not a mutable one, not a special
    -- one, none. That is what keeps §14's immutability absolute and lets
    -- replication, placement, integrity and GC stay free of special cases.
    source_class TEXT NOT NULL DEFAULT 'managed'
                 CHECK (source_class IN ('managed', 'linked', 'vault')),
    blob_hash    TEXT REFERENCES blobs (hash) ON DELETE RESTRICT,

    -- Where the bytes were found. For a linked asset this is where they still
    -- are; for a managed one it is provenance only and never identity (§61).
    source_path  TEXT,
    -- Detects change for linked assets. Explicitly not an identity and never
    -- addresses anything.
    fingerprint  TEXT,

    role        TEXT NOT NULL DEFAULT 'primary',
    filename    TEXT,
    mime        TEXT,
    -- Which rule produced this identification, so Milestone 3's real
    -- identifier can find these rows and re-resolve them (M1-11).
    identification_source TEXT NOT NULL DEFAULT 'unknown',
    missing_since TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,

    -- The invariant is the database's job, not the caller's.
    CHECK (
        (source_class = 'linked' AND blob_hash IS NULL AND source_path IS NOT NULL)
        OR (source_class IN ('managed', 'vault') AND blob_hash IS NOT NULL)
    )
) STRICT;

CREATE INDEX assets_by_edition ON assets (edition_id);
CREATE INDEX assets_by_blob ON assets (blob_hash) WHERE blob_hash IS NOT NULL;
CREATE INDEX assets_by_library ON assets (library_id);
-- One linked asset per path per library: re-scanning must converge, not multiply.
CREATE UNIQUE INDEX assets_linked_path ON assets (library_id, source_path)
    WHERE source_class = 'linked';

-- Convergent content state (§8) ------------------------------------------------
CREATE TABLE replicas (
    blob_hash     TEXT NOT NULL REFERENCES blobs (hash) ON DELETE CASCADE,
    peer_id       TEXT NOT NULL REFERENCES peers (id) ON DELETE CASCADE,
    state         TEXT NOT NULL DEFAULT 'present'
                  CHECK (state IN ('present', 'pending', 'corrupt', 'missing')),
    bytes_present INTEGER NOT NULL DEFAULT 0 CHECK (bytes_present >= 0),
    verified_at   TEXT,
    updated_at    TEXT NOT NULL,
    PRIMARY KEY (blob_hash, peer_id)
) STRICT;

CREATE INDEX replicas_by_peer_state ON replicas (peer_id, state);

-- Scanner fingerprint cache ----------------------------------------------------
-- The difference between a rescan taking seconds and taking hours: an unchanged
-- file must never be read (M1-12).
CREATE TABLE scanned_files (
    root_id    TEXT NOT NULL REFERENCES library_roots (id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    size       INTEGER NOT NULL,
    mtime_ns   INTEGER NOT NULL,
    dev        INTEGER NOT NULL DEFAULT 0,
    inode      INTEGER NOT NULL DEFAULT 0,
    blob_hash  TEXT REFERENCES blobs (hash) ON DELETE SET NULL,
    scanned_at TEXT NOT NULL,
    PRIMARY KEY (root_id, path)
) STRICT;

CREATE INDEX scanned_files_by_blob ON scanned_files (blob_hash) WHERE blob_hash IS NOT NULL;

-- Identity (ADR-0011) ----------------------------------------------------------
CREATE TABLE principals (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('user', 'service')),
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
) STRICT;

CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    -- argon2id, never the token itself.
    token_hash   TEXT NOT NULL,
    scopes       TEXT NOT NULL DEFAULT '' ,
    created_at   TEXT NOT NULL,
    last_used_at TEXT,
    expires_at   TEXT,
    revoked_at   TEXT
) STRICT;

CREATE INDEX api_tokens_by_principal ON api_tokens (principal_id);

-- +goose Down
DROP TABLE api_tokens;
DROP TABLE principals;
DROP TABLE scanned_files;
DROP TABLE replicas;
DROP TABLE assets;
DROP TABLE blobs;
DROP TABLE external_ids;
DROP TABLE editions;
DROP TABLE works;
DROP TABLE library_roots;
DROP TABLE libraries;
DROP TABLE peers;
