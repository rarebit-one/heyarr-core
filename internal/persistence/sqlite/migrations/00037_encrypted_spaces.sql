-- +goose Up
-- Encrypted personal-state storage: the Milestone 9 third plane's peer-side store
-- (§38, §39, §41, §79, ADR-0049). A peer holds encrypted spaces and the wrapped
-- copies of their keys, and it CANNOT read any of it — it stores ciphertext and
-- opaque metadata and holds no X25519 private key to unwrap with. This is the
-- storage half of the invariant SECURITY.md and Invariant 6 exist for.
--
-- Why this lives in the control-plane database at all: the peer is the single
-- writer of its OWN store (ADR-0003 holds — one writer per database), and what it
-- writes is what it received and cannot interpret. The multi-master property
-- (§43) is between DIFFERENT peers' stores converging CLIENT-side (§42), not two
-- writers on one database — so these are ordinary single-writer tables holding
-- opaque rows, not an active-active replica.

CREATE TABLE encrypted_spaces (
    id          TEXT PRIMARY KEY,           -- opaque UUIDv7 (ADR-0017)

    -- The §39 category (personal, family, shared, research). It is STRUCTURAL — a
    -- peer may see it — and is the only thing about a space, besides its
    -- existence, the peer is permitted to know. There is deliberately NO name
    -- column: a space's name is encrypted CRDT state under the space key, not
    -- metadata the peer stores (§38). Storing it here would leak it to every peer.
    kind        TEXT NOT NULL,

    created_at  TEXT NOT NULL
) STRICT;

CREATE TABLE wrapped_keys (
    id          TEXT PRIMARY KEY,           -- UUIDv7

    -- CASCADE: dropping a space takes its wrapped keys with it — a wrapped key
    -- for a space that no longer exists opens nothing.
    space_id    TEXT NOT NULL REFERENCES encrypted_spaces (id) ON DELETE CASCADE,

    -- The recipient this copy is wrapped FOR: a device or the recovery encryption
    -- key, rendered "x25519:<hex>". The peer learns WHICH keys can read a space —
    -- device-level co-membership, §38's acknowledged structural fact — never the
    -- space key itself. One wrap per (space, recipient): a recipient has exactly
    -- one current wrapped copy of a space's key.
    recipient   TEXT NOT NULL,

    -- The sealed bytes: e_pub ‖ nonce ‖ ciphertext (encryption.Seal output). The
    -- peer holds these and no X25519 private key, so it cannot unwrap any of them.
    -- A BLOB, not text: this is ciphertext, not an identifier.
    wrapped     BLOB NOT NULL,

    created_at  TEXT NOT NULL,

    UNIQUE (space_id, recipient)
) STRICT;

CREATE INDEX wrapped_keys_by_space ON wrapped_keys (space_id);

-- +goose Down
DROP INDEX wrapped_keys_by_space;
DROP TABLE wrapped_keys;
DROP TABLE encrypted_spaces;
