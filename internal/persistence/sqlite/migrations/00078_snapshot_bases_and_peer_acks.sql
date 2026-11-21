-- +goose Up
-- Snapshot trust (#681, ADR-0101 amendment): the compaction base and the
-- durable acknowledgements that prove a compaction safe. Both are opaque causal
-- metadata, like encrypted_snapshots.frontier; neither names any content (§38).

-- A space's compaction base: the snapshot a compaction named and pinned. One per
-- space. A cold-starting device prefers it unless a newer snapshot covers it, and
-- it replicates to every trusted Full Peer as §45's compaction metadata. A new
-- base replaces the old only when it covers it.
CREATE TABLE snapshot_bases (
    space_id    TEXT PRIMARY KEY REFERENCES encrypted_spaces (id) ON DELETE CASCADE,
    -- RESTRICT: a pinned snapshot is never deleted while it is the base.
    snapshot_id TEXT NOT NULL REFERENCES encrypted_snapshots (snapshot_id) ON DELETE RESTRICT,
    pinned_at   TEXT NOT NULL
) STRICT;

-- What each trusted Full Peer confirmed holding, per space, as of its last
-- audited reconcile: every change in the causal history of frontier (over this
-- node's log), and the base it had pinned. Written only after the target
-- confirmed each id itself (POST /peer/v1/state/{space}/held), never from the
-- heads it offers. Compaction drops nothing outside the acked history of every
-- trusted Full Peer.
CREATE TABLE state_peer_acks (
    space_id         TEXT NOT NULL REFERENCES encrypted_spaces (id) ON DELETE CASCADE,
    peer_id          TEXT NOT NULL,
    -- Comma-joined change ids, as in encrypted_snapshots.frontier.
    frontier         TEXT NOT NULL DEFAULT '',
    -- The base the peer had pinned when it confirmed; '' when it had none.
    base_snapshot_id TEXT NOT NULL DEFAULT '',
    acked_at         TEXT NOT NULL,
    PRIMARY KEY (space_id, peer_id)
) STRICT;

-- +goose Down
DROP TABLE state_peer_acks;
DROP TABLE snapshot_bases;
