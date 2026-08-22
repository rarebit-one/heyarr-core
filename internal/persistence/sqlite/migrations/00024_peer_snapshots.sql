-- +goose Up
-- peer_snapshots: the control plane's record of what each Full Peer holds as a
-- materialised catalog snapshot (§52, §79, M4-13).
--
-- # What this table is NOT
--
-- It is not the snapshot. The snapshot is a SEPARATE DATABASE FILE on the peer
-- (internal/peer/catalog), written by one builder and opened read-only by
-- everything that reads it. §52 says the snapshot "should not be treated as
-- independently writable control state", and a shadow schema in this database
-- would be one careless UPDATE away from being exactly that — a second writer
-- against the control plane, which Invariant 5 and ADR-0003 forbid outright.
--
-- So the split is: the bytes of the snapshot live on the peer, in their own
-- file, with their own lifecycle; this table is the CONTROLLER's index of them
-- — which version it issued to whom, when, and what it contained. §79 lists
-- peer_snapshots among the control-plane tables, and this is why: an operator
-- asking "how stale is Site B?" is asking the control plane, not Site B.
--
-- # Why the version is allocated here
--
-- A snapshot version must increase monotonically, and the only place that can
-- be guaranteed is the single writer (ADR-0003). A peer allocating its own
-- would be two allocators the moment a second peer existed, and "monotonic"
-- would quietly become "monotonic per peer, per process, until a restart".
-- The controller allocates it inside the transaction that records it.
--
-- # Why generated_at is stored rather than derived
--
-- Age is the whole point (§53: "conservative rather than unavailable"). A
-- stale answer presented as current is worse than an unavailable one, so the
-- moment the snapshot describes is a recorded fact rather than something
-- inferred from when a row was last touched. updated_at moves when the
-- controller writes; generated_at is when the catalogue was read.
CREATE TABLE peer_snapshots (
    -- One row per peer. A peer holds one snapshot: the current one. History is
    -- the event log's job (catalog.snapshot_built), not this table's — keeping
    -- every version here would grow without bound and answer a question
    -- nobody asks.
    peer_id        TEXT PRIMARY KEY REFERENCES peers (id) ON DELETE CASCADE,

    -- Which controller this snapshot came from. Recorded even though there is
    -- exactly one controller (ADR-0029), because the field a recovery reads
    -- (§51, §82) must say whose catalogue it is looking at — a snapshot
    -- restored from a backup of a different deployment is the failure that
    -- makes this a column rather than an assumption.
    controller_id  TEXT NOT NULL,

    -- Monotonic per peer. CHECK (> 0) so that "no snapshot" cannot be spelled
    -- as version 0: absent is the absence of a row, and the two must never be
    -- the same answer (§53 — "the library is empty" and "I cannot help you"
    -- are different sentences).
    version        INTEGER NOT NULL CHECK (version > 0),

    -- When the controller READ the catalogue, not when it wrote this row.
    generated_at   TEXT NOT NULL,

    -- Whether the peer received every row or only what changed. Recorded
    -- because a full rebuild is the drift corrector (the same discipline
    -- M4-07 applies to inventory), and "when did we last take the slow, safe
    -- path?" is an operational question.
    kind           TEXT NOT NULL CHECK (kind IN ('full', 'incremental')),

    -- The high-water mark the NEXT incremental refresh asks from. Stored
    -- rather than reusing generated_at so the two can diverge if the source
    -- ever needs a conservative overlap — which it does today: the source
    -- selects rows at or after the watermark, deliberately re-sending a row
    -- rather than risking dropping one.
    watermark      TEXT NOT NULL,

    -- How many rows the snapshot contains, across every covered table. An
    -- operator-facing sanity number; the authority on contents is the digest.
    row_count      INTEGER NOT NULL CHECK (row_count >= 0),

    -- A content fingerprint over the covered tables, canonicalised and hashed.
    -- It is what makes "an incremental refresh and a full rebuild of the same
    -- catalogue state produce identical snapshots" an assertion rather than a
    -- hope, and what lets a peer and a controller compare snapshots without
    -- shipping one.
    content_digest TEXT NOT NULL,

    updated_at     TEXT NOT NULL
) STRICT;

-- "Which peers are stale?" is the question this table exists to answer, and it
-- is asked by age rather than by peer.
CREATE INDEX peer_snapshots_by_age ON peer_snapshots (generated_at);

-- +goose Down
DROP INDEX peer_snapshots_by_age;
DROP TABLE peer_snapshots;
