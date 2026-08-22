-- +goose Up
-- Inventory reports and replica freshness (§19, §20, §21, ADR-0029, M4-07).
--
-- # Why this migration exists at all
--
-- `replicas` already looks like an inventory: (blob_hash, peer_id, state,
-- bytes_present, verified_at). It is not one. It is what the CONTROLLER
-- believes; a peer's inventory is what is on its DISK. Every writer of
-- `replicas` up to now resolves the self peer first
-- (internal/persistence/catalog/integrity.go), so no row has ever described a
-- machine other than the one that wrote it, and the two things have never had
-- the opportunity to disagree. A second Full Peer is what creates that
-- opportunity: a peer that lost a disk, restored an older CAS, or quarantined
-- a blob holds an inventory this table does not reflect.
--
-- §21 puts verification on the destination because the ground truth about
-- bytes is the bytes. This migration makes `replicas` an honest controller-side
-- CACHE of peer-reported reality rather than one by accident.
--
-- # replicas.reported_at — freshness
--
-- verified_at is when the bytes were last re-hashed. reported_at is when the
-- peer that holds them last CONFIRMED this row in an inventory report. They
-- are different facts and the difference is the point: a peer can confirm it
-- still holds a blob far more often than it can afford to re-hash it, and a
-- row that has not been confirmed recently is a fact about the past.
--
-- M4-12's garbage collection reads this before deleting what it believes is a
-- surplus copy. A table that could not express "nobody has confirmed this
-- lately" would let GC treat a decade-old belief as a live replica.
--
-- It is deliberately NOT backfilled. NULL means exactly "no peer has ever
-- confirmed this row through an inventory report", which is true of every row
-- that exists when this migration runs, and inventing a confirmation time from
-- verified_at or updated_at would manufacture the one fact this column exists
-- to make unfakeable. The self peer reports through the same mechanism as any
-- other peer, so its rows acquire a real reported_at on its first cycle.
ALTER TABLE replicas ADD COLUMN reported_at TEXT;

-- Freshness is a range scan per peer: "what has peer X not confirmed since T".
-- replicas_by_peer_state already covers "what does peer X hold"; neither index
-- serves the other's question.
CREATE INDEX replicas_by_peer_reported ON replicas (peer_id, reported_at);

-- # inventory_reports — the receipt log
--
-- One row per report CYCLE, not per blob. It is the durable answer to "when
-- did this peer last tell us anything, and what did it say" — which is a
-- different question from "what does it hold", and one that `replicas` cannot
-- answer at all once a report has been folded in.
--
-- It exists because the failure this whole issue is about is silent: a peer
-- that stops reporting leaves a `replicas` table that keeps claiming the
-- library is safe. A peer that stops reporting leaves NO new rows here, and
-- that absence is visible.
--
-- mode distinguishes the two shapes a report takes. An incremental report
-- carries only what changed since the peer's previous cycle; a full report
-- carries everything the peer holds and is the drift corrector — a blob absent
-- from a full report is a blob that peer no longer has.
CREATE TABLE inventory_reports (
    id          TEXT PRIMARY KEY,           -- UUIDv7 (ADR-0017)
    peer_id     TEXT NOT NULL REFERENCES peers (id) ON DELETE CASCADE,
    mode        TEXT NOT NULL CHECK (mode IN ('full', 'incremental')),
    -- When the peer observed its disk, as the peer measured it. Distinct from
    -- received_at, which is the controller's clock: the gap between them is
    -- how stale the report already was when it arrived.
    observed_at TEXT NOT NULL,
    received_at TEXT NOT NULL,
    -- What the report contained and what folding it in did. Counts rather than
    -- rows: the per-blob facts are in `replicas`, which is where anything
    -- wanting the detail must look.
    entries     INTEGER NOT NULL DEFAULT 0 CHECK (entries >= 0),
    added       INTEGER NOT NULL DEFAULT 0 CHECK (added >= 0),
    changed     INTEGER NOT NULL DEFAULT 0 CHECK (changed >= 0),
    -- removed is replicas rows this report moved to 'missing'. It is counted
    -- separately from `changed` because it is the only direction that makes
    -- the library less safe, and an operator scanning this table is looking
    -- for exactly that column.
    removed     INTEGER NOT NULL DEFAULT 0 CHECK (removed >= 0),
    -- Entries naming a blob this controller has no row for. Not an error: a
    -- peer restored from a newer catalog legitimately holds bytes this
    -- controller has not learned about yet. It cannot be recorded as a replica
    -- of a blob that does not exist, so it is counted and reported.
    unknown     INTEGER NOT NULL DEFAULT 0 CHECK (unknown >= 0)
) STRICT;

-- "What has this peer told us lately", newest first.
CREATE INDEX inventory_reports_by_peer ON inventory_reports (peer_id, received_at DESC);

-- +goose Down
DROP INDEX inventory_reports_by_peer;
DROP TABLE inventory_reports;
DROP INDEX replicas_by_peer_reported;
ALTER TABLE replicas DROP COLUMN reported_at;
