-- +goose NO TRANSACTION
-- +goose Up
-- Release intent for vault blobs, and the `released` durability basis (#779,
-- ADR-0114).
--
-- # blob_releases
--
-- On a two-site fabric, GC's last-copy rule (ADR-0018) spares an unpinned vault
-- blob forever at whichever site reclaims second: once the other copy has gone,
-- no other peer claims it and the sweep reports `no_other_peer` on every pass.
-- Nothing is lost, but every vault deletion leaves its bytes at one site.
--
-- A row here records that the last thing wanting a vault blob let it go: its
-- last placement pin was removed (`unpinned`), or the sibling it was taken from
-- withdrew the placement (`withdrawn`). On the personal profile every reference
-- to a vault blob is a pin or a taken placement, so "nothing pins it anywhere"
-- is "nobody wants it". GC on that profile may then reclaim the last copy after
-- the grace window, without the durability gate, and records why (`released`).
--
-- Only vault blobs are ever released: the row is written only when no asset and
-- no scanned library file reaches the blob, so library media never carries one.
-- Unlisting a sibling (DropPlacementsFromOthers) records nothing; that is an
-- operator changing topology, not an owner letting go.
--
-- No foreign key, for the reason 00050 gives for placement_pins: the row must
-- not depend on churn in `blobs`. Reclaim removes it with the blob, and a new
-- pin, placement or reference clears it.
--
-- # durability_evidence.basis gains 'released'
--
-- It also gains released_at and unreferenced_since: for a released reclaim,
-- the generation the sweep decided on, which the reclaim transaction compares
-- with the current release and mark.
--
-- The evidence row is what Reclaim's defence in depth requires before it will
-- delete a tracked blob (00028). A released blob's evidence names that basis, so
-- an operator can read why a last copy went. SQLite cannot alter a table-level
-- CHECK, so this is the rebuild 00051 uses, with foreign_keys OFF (hence NO
-- TRANSACTION). Nothing references durability_evidence and it references
-- nothing.

PRAGMA foreign_keys = OFF;

CREATE TABLE blob_releases (
    blob_hash      TEXT PRIMARY KEY
                   CHECK (blob_hash GLOB 'blake3:[0-9a-f]*' AND length(blob_hash) = 71),
    cause          TEXT NOT NULL CHECK (cause IN ('unpinned', 'withdrawn')),
    -- The sibling that withdrew the placement, for `withdrawn`. Empty otherwise.
    source_peer_id TEXT NOT NULL DEFAULT '',
    released_at    TEXT NOT NULL
) STRICT;

CREATE TABLE durability_evidence_new (
    id          TEXT PRIMARY KEY,
    blob_hash   TEXT NOT NULL
                CHECK (blob_hash GLOB 'blake3:[0-9a-f]*' AND length(blob_hash) = 71),
    peer_id     TEXT NOT NULL DEFAULT '',
    peer_name   TEXT NOT NULL DEFAULT '',
    endpoint    TEXT NOT NULL DEFAULT '',
    basis       TEXT NOT NULL CHECK (basis IN ('verified_remote', 'sole_peer', 'released')),
    reported_at TEXT,
    verified_at TEXT,
    size        INTEGER NOT NULL DEFAULT 0 CHECK (size >= 0),
    detail      TEXT NOT NULL DEFAULT '',
    recorded_at TEXT NOT NULL,
    -- For `released` only: the release and the grace-window mark the sweep
    -- observed. The reclaim refuses unless both are still exactly current, so
    -- a newer release generation is never deleted on an older one's window.
    released_at        TEXT,
    unreferenced_since TEXT
) STRICT;

INSERT INTO durability_evidence_new
    (id, blob_hash, peer_id, peer_name, endpoint, basis, reported_at, verified_at, size, detail, recorded_at)
SELECT id, blob_hash, peer_id, peer_name, endpoint, basis, reported_at, verified_at, size, detail, recorded_at
FROM durability_evidence;

DROP TABLE durability_evidence;
ALTER TABLE durability_evidence_new RENAME TO durability_evidence;

CREATE INDEX durability_evidence_by_blob ON durability_evidence (blob_hash);

PRAGMA foreign_keys = ON;

-- +goose NO TRANSACTION
-- +goose Down
-- Back to two bases. Evidence for a released reclaim cannot exist in the
-- narrower CHECK, so the copy drops it; the blob it describes is already gone.

PRAGMA foreign_keys = OFF;

CREATE TABLE durability_evidence_old (
    id          TEXT PRIMARY KEY,
    blob_hash   TEXT NOT NULL
                CHECK (blob_hash GLOB 'blake3:[0-9a-f]*' AND length(blob_hash) = 71),
    peer_id     TEXT NOT NULL DEFAULT '',
    peer_name   TEXT NOT NULL DEFAULT '',
    endpoint    TEXT NOT NULL DEFAULT '',
    basis       TEXT NOT NULL CHECK (basis IN ('verified_remote', 'sole_peer')),
    reported_at TEXT,
    verified_at TEXT,
    size        INTEGER NOT NULL DEFAULT 0 CHECK (size >= 0),
    detail      TEXT NOT NULL DEFAULT '',
    recorded_at TEXT NOT NULL
) STRICT;

INSERT INTO durability_evidence_old
    (id, blob_hash, peer_id, peer_name, endpoint, basis, reported_at, verified_at, size, detail, recorded_at)
SELECT id, blob_hash, peer_id, peer_name, endpoint, basis, reported_at, verified_at, size, detail, recorded_at
FROM durability_evidence
WHERE basis <> 'released';

DROP TABLE durability_evidence;
ALTER TABLE durability_evidence_old RENAME TO durability_evidence;

CREATE INDEX durability_evidence_by_blob ON durability_evidence (blob_hash);

DROP TABLE blob_releases;

PRAGMA foreign_keys = ON;
