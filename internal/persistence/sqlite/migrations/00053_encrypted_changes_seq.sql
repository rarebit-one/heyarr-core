-- +goose Up
-- A monotonic per-peer arrival sequence for encrypted changes, so a device can
-- pull INCREMENTALLY (§44) instead of re-fetching the whole log on every sync.
--
-- Why a new column and not created_at: created_at is RFC3339Nano, which is NOT
-- lexicographically ordered — "…:00Z" sorts AFTER "…:00.1Z" because 'Z' > '.'.
-- Ordering by it is therefore subtly wrong already, and a cursor built on it
-- would SKIP changes, which for a CRDT log means silent divergence. seq is an
-- integer, so its order is its value.
--
-- Why not rowid: VACUUM may renumber rowids on a table without an INTEGER
-- PRIMARY KEY, which would rewind or scramble every device's cursor. seq is an
-- ordinary column VACUUM never touches.
--
-- The peer is the single writer of its own store (ADR-0003, Invariant 5), so
-- assigning seq as MAX(seq)+1 inside the insert transaction is safe: there is no
-- concurrent writer to race. seq is LOCAL arrival order on THIS peer, not a
-- causal or cross-peer order — a change replicated from another peer is appended
-- at the end, never inserted behind a cursor a device has already passed. That is
-- exactly the property an append-only cursor needs, and it is why the cursor is
-- opaque to the client and never comparable across peers.

ALTER TABLE encrypted_changes ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;

-- Backfill in the existing arrival order. rowid is insertion order here and is
-- stable for the duration of this migration, so it is the best available
-- reconstruction of arrival for rows stored before seq existed.
UPDATE encrypted_changes SET seq = rowid;

CREATE INDEX encrypted_changes_by_space_seq ON encrypted_changes (space_id, seq);

-- +goose Down
DROP INDEX encrypted_changes_by_space_seq;
ALTER TABLE encrypted_changes DROP COLUMN seq;
