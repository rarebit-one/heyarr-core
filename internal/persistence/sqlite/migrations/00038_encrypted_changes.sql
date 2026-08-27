-- +goose Up
-- Encrypted CRDT changes: the third thing a peer holds for the personal-state
-- plane and cannot read, alongside encrypted_spaces and wrapped_keys (§42, §44,
-- §79, ADR-0049). A change is stored under its CONTENT-ADDRESSED id (the
-- protocol's "blake3:<hex>" over space, parents and ciphertext), so a peer
-- accepting one verifies the id against the bytes and never trusts a claimed one
-- (Invariant 1, ADR-0005). The peer sees the id, the space, the causal parents
-- that route the change, and the ciphertext — never the plaintext (§38).
--
-- This is the STORAGE for the state-sync protocol (§44), which is distinct from
-- CAS sync: its unit is a small encrypted causal change, not a large blob. The
-- peer is the single writer of its own store (ADR-0003 holds); convergence across
-- peers is client-side (§42), so these are ordinary single-writer opaque rows.

CREATE TABLE encrypted_changes (
    -- The content-addressed change id, "blake3:<hex>" (protocol.EncryptedChange).
    -- PRIMARY KEY so accepting the same change twice is idempotent — a lossy or
    -- malicious relay re-sending a change cannot duplicate it.
    change_id  TEXT PRIMARY KEY,

    -- CASCADE: dropping a space drops its changes — a change for a space that no
    -- longer exists routes nowhere.
    space_id   TEXT NOT NULL REFERENCES encrypted_spaces (id) ON DELETE CASCADE,

    -- The causal parents, comma-joined change ids (each "blake3:<hex>", so no
    -- comma can occur inside one). Empty for a root change. This is the causal
    -- metadata that ORDERS changes without reading them (§44) — the peer routes by
    -- it, the client merges by it.
    parents    TEXT NOT NULL DEFAULT '',

    -- The encrypted CRDT change, opaque to the peer. A BLOB, not text: ciphertext.
    ciphertext BLOB NOT NULL,

    created_at TEXT NOT NULL
) STRICT;

CREATE INDEX encrypted_changes_by_space ON encrypted_changes (space_id, created_at);

-- +goose Down
DROP INDEX encrypted_changes_by_space;
DROP TABLE encrypted_changes;
