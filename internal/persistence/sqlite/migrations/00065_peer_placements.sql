-- +goose Up
-- Placements taken from a sibling (ADR-0114): blobs a sibling site holds and
-- offered this node, which this node keeps as its own desire and pulls. They are
-- the destination's half of two-site vault durability. The source never writes
-- here: this node ASKS each sibling what it offers (POST /peer/v1/placements),
-- so the question and the fetch both start at the destination (ADR-0038).
--
-- A row is unioned into convergence as a SELF-pin and counted by garbage
-- collection as a reference, exactly as a placement_pins row naming this node is.
-- It is kept apart from placement_pins for one reason: a row here is never
-- offered on. A blob this node took from a sibling is not this node's to offer
-- back, or two siblings would keep each other's pins alive after the origin
-- dropped it.
--
-- Replaced per hash range by each exchange (ADR-0114), so a placement the source
-- withdrew is a row deleted here, and its bytes then go only through the GC
-- grace window (ADR-0018). Like the source's replica, the row is about a member:
-- removing the peer removes what it offered, and the bytes follow by GC. So does
-- removing it from placement.siblings: a node drops what it took from peers it no
-- longer lists, at start.
CREATE TABLE peer_placements (
    source_peer_id TEXT NOT NULL REFERENCES peers (id) ON DELETE CASCADE,
    blob_hash      TEXT NOT NULL CHECK (blob_hash GLOB 'blake3:[0-9a-f]*' AND length(blob_hash) = 71),
    size           INTEGER NOT NULL CHECK (size >= 0),
    -- Whether the source said it holds the bytes. A source that lost its copy
    -- keeps offering the blob with this false: still wanted, not servable.
    source_held    INTEGER NOT NULL DEFAULT 1 CHECK (source_held IN (0, 1)),
    received_at    TEXT NOT NULL,
    PRIMARY KEY (source_peer_id, blob_hash)
) STRICT;

CREATE INDEX peer_placements_by_blob ON peer_placements (blob_hash);

-- +goose Down
DROP INDEX peer_placements_by_blob;
DROP TABLE peer_placements;
