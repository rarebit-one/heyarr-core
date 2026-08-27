-- +goose Up
-- Encrypted snapshots and compaction (§44, ADR-0049): the fourth opaque thing a
-- peer holds for the personal-state plane, alongside encrypted_spaces,
-- wrapped_keys and encrypted_changes. A snapshot is a materialised CRDT state at
-- a causal point, encrypted under the space key — so a joining or long-offline
-- device fetches a bounded SNAPSHOT + the tail of changes after it rather than
-- replaying the whole log. The peer stores it as ciphertext and opaque causal
-- metadata (the frontier it covers) and never materialises it (§38).
--
-- Compaction — dropping changes a snapshot subsumes — is not a schema change: it
-- is a DELETE from encrypted_changes bounded by the frontier every replica holds
-- (store.CompactChanges). The snapshot is what makes that safe: the dropped
-- changes are recoverable from it, and only changes inside the acknowledged
-- frontier are ever dropped, so a change a partitioned peer still needs survives.

CREATE TABLE encrypted_snapshots (
    -- The content-addressed snapshot id, "blake3:<hex>" over the space, the
    -- frontier and the ciphertext (protocol.EncryptedSnapshot). PRIMARY KEY so
    -- accepting the same snapshot twice is idempotent.
    snapshot_id TEXT PRIMARY KEY,

    -- CASCADE: dropping a space drops its snapshots.
    space_id    TEXT NOT NULL REFERENCES encrypted_spaces (id) ON DELETE CASCADE,

    -- The causal frontier this snapshot materialises, comma-joined change ids
    -- (each "blake3:<hex>", so no comma occurs inside one). This is the opaque
    -- causal metadata that says WHICH point the snapshot folded to — the peer
    -- bounds sync by it, the client resumes the tail after it — never any content.
    frontier    TEXT NOT NULL DEFAULT '',

    -- The encrypted materialised state, opaque to the peer. A BLOB: ciphertext.
    ciphertext  BLOB NOT NULL,

    created_at  TEXT NOT NULL
) STRICT;

CREATE INDEX encrypted_snapshots_by_space ON encrypted_snapshots (space_id, created_at);

-- +goose Down
DROP INDEX encrypted_snapshots_by_space;
DROP TABLE encrypted_snapshots;
