-- +goose Up
-- Why garbage collection believed a blob was safe to unlink (ADR-0018, §19,
-- §53, §56, M4-12).
--
-- # The problem this table exists to solve
--
-- `replicas.blob_hash REFERENCES blobs (hash) ON DELETE CASCADE`
-- (00002_core.sql). Catalog.Reclaim deletes the `blobs` row, so the record of
-- who ELSE held the blob is destroyed by the very transaction that decided
-- deleting it was safe. After a reclaim there is nothing left to answer the
-- only question anybody asks afterwards: on what grounds?
--
-- So the grounds are written down first, into a table that survives the delete.
-- The precedent is `quarantine` (00007_integrity.sql), which keeps the evidence
-- for a blob whose bytes failed verification for exactly the same reason.
--
-- # No foreign keys, deliberately
--
-- blob_hash has no reference to `blobs`: the row it describes is about to be
-- deleted, and a cascade would take the evidence with it — reintroducing the
-- bug this table was added to close. peer_id has no reference to `peers` for
-- the same shape of reason: revocation is deletion (ADR-0012), and a peer
-- being removed six months later must not erase the record of the copy it was
-- holding when a blob was unlinked here.
--
-- The cost is that neither column is constrained to a row that exists. That is
-- the correct trade: this is a ledger, not a relation. Its whole value is that
-- it keeps saying something true after everything it names has gone.
--
-- # basis — what was established, not merely believed
--
--   verified_remote  another peer was asked, over the peer fabric, whether it
--                    held these bytes, and answered that it did. The `replicas`
--                    row was the reason to ask; it was never the answer.
--   sole_peer        this deployment has exactly one peer, so there is no
--                    "elsewhere" for a placement policy to be satisfied at. The
--                    four preconditions of ADR-0018 are the whole gate, and
--                    this row records that that is the ground being relied on.
--
-- A third value is deliberately absent: there is no `replica_row` basis, no
-- "the catalog said so". A belief is a reason to check, and this milestone's
-- premise is that beliefs and bytes diverge.
--
-- # reported_at and verified_at travel with it
--
-- Freshness is the difference between a fact and a fact about the past. An
-- evidence row that recorded only "peer B had it" would be unfalsifiable later;
-- one that records when peer B last CONFIRMED it (reported_at, 00023) and when
-- those bytes were last re-hashed (verified_at) can be argued with.
CREATE TABLE durability_evidence (
    id          TEXT PRIMARY KEY,           -- UUIDv7 (ADR-0017)
    blob_hash   TEXT NOT NULL
                CHECK (blob_hash GLOB 'blake3:[0-9a-f]*' AND length(blob_hash) = 71),
    -- The peer the bytes were established to be on. Empty for sole_peer, where
    -- the point is that there is no other peer.
    peer_id     TEXT NOT NULL DEFAULT '',
    -- Denormalised on purpose: the evidence must stay readable after the peer
    -- row is gone, and a dangling id is not readable.
    peer_name   TEXT NOT NULL DEFAULT '',
    endpoint    TEXT NOT NULL DEFAULT '',
    basis       TEXT NOT NULL CHECK (basis IN ('verified_remote', 'sole_peer')),
    -- When that peer last confirmed this replica in an inventory report, and
    -- when the bytes were last re-hashed. Both may be NULL: NULL means nobody
    -- ever did, which is a different statement from "long ago".
    reported_at TEXT,
    verified_at TEXT,
    size        INTEGER NOT NULL DEFAULT 0 CHECK (size >= 0),
    detail      TEXT NOT NULL DEFAULT '',
    recorded_at TEXT NOT NULL
) STRICT;

-- "On what grounds was this blob unlinked" is the only question asked of this
-- table, and it is always asked by hash.
CREATE INDEX durability_evidence_by_blob ON durability_evidence (blob_hash);

-- +goose Down
DROP INDEX durability_evidence_by_blob;
DROP TABLE durability_evidence;
