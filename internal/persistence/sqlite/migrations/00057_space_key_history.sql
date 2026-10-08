-- +goose Up
-- A space key gains an EPOCH and an opaque history, so rotating a space's key no
-- longer strands the content sealed under the keys before it (ADR-0103, #698).
--
-- # The history row
--
-- Epoch 0 is the key a space was created with. Rotating to epoch N stores ONE
-- row: key_{N-1} sealed under key_N by the client that rotated. The peer cannot
-- open it (Invariant 6) — it is the same kind of opaque bytes as a wrapped key,
-- only sealed with a symmetric space key instead of to a device's X25519 key. A
-- current recipient unwraps key_N, opens row N to get key_{N-1}, and so on back
-- to epoch 0, which is how pre-rotation content stays readable.
--
-- (space_id, epoch) is the primary key: there is exactly one predecessor per
-- epoch, so two rotations racing to the same epoch cannot both land and fork the
-- chain — the second is a conflict, not a second row.
--
-- epoch >= 1: epoch 0 has no predecessor, so it has no row.
--
-- CASCADE: dropping a space takes its history with it, like its wrapped keys.
CREATE TABLE space_key_history (
    space_id     TEXT    NOT NULL REFERENCES encrypted_spaces (id) ON DELETE CASCADE,
    epoch        INTEGER NOT NULL CHECK (epoch >= 1),

    -- key_{epoch-1} sealed under key_{epoch} (AEAD output). A BLOB: ciphertext,
    -- not an identifier. The peer stores it and cannot open it.
    sealed_prev  BLOB    NOT NULL,

    created_at   TEXT    NOT NULL,

    PRIMARY KEY (space_id, epoch)
) STRICT;

-- The epoch a wrapped copy seals. There is deliberately NO current-epoch column
-- on encrypted_spaces: a space's current epoch is DERIVED — MAX(epoch) of its
-- history rows, 0 when it has none — so it cannot drift from the history it
-- summarises. A wrap below the current epoch is stale and is refused on write
-- and deleted when a rotation lands; that is what stops replication resurrecting
-- an old or revoked copy (last-write-wins with no delete replication did).
--
-- DEFAULT 0: every wrap stored before this migration seals the space's original
-- key, because no space had a history row yet.
ALTER TABLE wrapped_keys ADD COLUMN epoch INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE wrapped_keys DROP COLUMN epoch;
DROP TABLE space_key_history;
