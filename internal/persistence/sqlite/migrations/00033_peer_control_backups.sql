-- +goose Up
-- What generation of THIS node's control-plane backup each peer is believed to
-- hold (§50, M7-03, ADR-0046).
--
-- # A belief, not a fact
--
-- This table is the controller's record of the last backup it successfully
-- pushed to each peer. It is a BELIEF, in the precise sense internal/peer/durability
-- means it (M4-12): what the controller thinks a machine it is not is holding.
-- The FACT is on the peer's own disk, and the peer answers it over
-- GET /peer/v1/control-backup. When the two disagree the peer wins — a peer
-- whose disk was wiped holds nothing, whatever this table remembers.
--
-- # Why store the belief at all, if the peer is authoritative
--
-- Because the peer cannot always be asked. The question this table exists to
-- answer is "is peer B a generation behind?", and it is asked precisely when B
-- is unreachable — during the incident, which is when a backup matters. A
-- controller that could only report what a reachable peer confirmed would go
-- silent about a peer exactly when its staleness is the thing worth knowing. So
-- the last confirmed push is remembered here, survives a controller restart, and
-- is reconciled against the peer's own answer whenever the peer can be reached.
--
-- # One row per peer, about OUR control plane
--
-- The generation is of this node's own control plane — the thing this node backs
-- up and pushes out. A peer holds backups from several sources under the
-- peer-repo model (ADR-0038); this table is only about what it holds of OURS,
-- which is the only thing this node pushes and the only belief it can form.
CREATE TABLE peer_control_backups (
    -- The peer this node pushed to. It is the membership id, and the row is
    -- meaningless once the peer is removed — which is why it cascades.
    peer_id TEXT NOT NULL
        REFERENCES peers (id) ON DELETE CASCADE,

    -- The generation this node last confirmed the peer stored. Monotonic for a
    -- given peer, because a backup generation is (ADR-0044): a push that would
    -- move it backwards is a bug, not a fact to record.
    generation INTEGER NOT NULL CHECK (generation > 0),

    -- The digest of that backup's snapshot, so "did the bytes that crossed match
    -- the bytes we took?" is answerable without the peer.
    digest TEXT NOT NULL,

    -- When the push was confirmed. Not when the backup was taken — that is in the
    -- manifest — but when this peer acknowledged holding it.
    pushed_at TEXT NOT NULL,

    PRIMARY KEY (peer_id)
) STRICT;

-- +goose Down
DROP TABLE peer_control_backups;
