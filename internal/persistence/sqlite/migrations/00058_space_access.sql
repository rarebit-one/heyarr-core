-- +goose NO TRANSACTION
-- +goose Up
-- Restricted principals and the per-space access list (ADR-0104).
--
-- An EXECUTOR is a service that runs work on an owner's behalf against a few
-- encrypted spaces and nothing else: it is not a member device (it holds no
-- enrolment cert and never wraps a key), and it must not inherit the node-wide
-- read+write a bearer token carries today. Four changes make that expressible:
--
--   1. principals.kind gains 'executor'.
--   2. api_tokens.restricted marks a token confined to the vault surface. 0 for
--      every existing row, so every token minted before this migration keeps
--      exactly the authority it had.
--   3. encrypted_spaces.owner_principal_id records which user created a space.
--      NULL for every existing space: a legacy household space, whose behaviour
--      for user principals is unchanged. Opaque like the rest of the row — a
--      principal id, never a name (§38).
--   4. space_grants is the fetch gate (ADR-0049's "may I fetch these ciphertext
--      changes"): one row per (space, principal), created and revoked only from
--      a management-authorised device. It says nothing about decryption — that
--      is still a wrapped key the grantee must separately hold.
--
-- # Why a rebuild of principals
--
-- SQLite cannot alter a table-level CHECK, and principals.kind carries one
-- (00002). Extending the allowed set is the twelve-step rebuild 00046 and 00051
-- use. api_tokens and user_identities reference principals, so the rebuild runs
-- with foreign_keys OFF (untoggleable inside a transaction — hence NO
-- TRANSACTION); with it OFF, dropping the old table cascades nothing, and the
-- renamed table keeps the name every reference already spells.

PRAGMA foreign_keys = OFF;

CREATE TABLE principals_new (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('user', 'service', 'executor')),
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
) STRICT;

INSERT INTO principals_new (id, kind, name, created_at)
SELECT id, kind, name, created_at FROM principals;

DROP TABLE principals;
ALTER TABLE principals_new RENAME TO principals;

-- 1 = confined to the vault surface (ADR-0104). An INTEGER rather than a CHECKed
-- boolean so a future DROP COLUMN stays possible; the store writes only 0 or 1.
ALTER TABLE api_tokens ADD COLUMN restricted INTEGER NOT NULL DEFAULT 0;

-- Deliberately no foreign key: a deleted owner leaves a space nobody can grant
-- on (fail closed), rather than a NULL owner that would read as a legacy space
-- any authorised device may grant on.
ALTER TABLE encrypted_spaces ADD COLUMN owner_principal_id TEXT;

CREATE TABLE space_grants (
    space_id             TEXT NOT NULL REFERENCES encrypted_spaces (id) ON DELETE CASCADE,
    principal_id         TEXT NOT NULL REFERENCES principals (id) ON DELETE CASCADE,

    -- What the grantee may do with the space's ciphertext. 'read' fetches keys
    -- (its own wrap only), history, changes and snapshots; 'read,write' may also
    -- push changes and snapshots. Never rotate, compact or delete a key.
    caps                 TEXT NOT NULL CHECK (caps IN ('read', 'read,write')),

    -- Who granted it: the user principal and the device key whose
    -- management-authorised credential made the request (ADR-0065).
    granted_by_principal TEXT NOT NULL,
    granted_by_device    TEXT NOT NULL,
    granted_at           TEXT NOT NULL,

    -- Optional standing-consent bound, enforced on fetch (plan risk R2). NULL is
    -- no expiry.
    expires_at           TEXT,

    -- A revoke keeps the row (the audit trail is the event log, but the row
    -- says when the gate closed); a re-grant clears it.
    revoked_at           TEXT,

    PRIMARY KEY (space_id, principal_id)
) STRICT;

CREATE INDEX space_grants_by_principal ON space_grants (principal_id);

PRAGMA foreign_keys = ON;

-- +goose NO TRANSACTION
-- +goose Down
-- Back to two principal kinds. An executor principal cannot exist under the
-- narrower CHECK, so it and its tokens are dropped — on a clean rollback there
-- are none.

PRAGMA foreign_keys = OFF;

DROP TABLE space_grants;
ALTER TABLE encrypted_spaces DROP COLUMN owner_principal_id;

DELETE FROM api_tokens WHERE principal_id IN (SELECT id FROM principals WHERE kind = 'executor');
ALTER TABLE api_tokens DROP COLUMN restricted;

CREATE TABLE principals_old (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('user', 'service')),
    name       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
) STRICT;

INSERT INTO principals_old (id, kind, name, created_at)
SELECT id, kind, name, created_at FROM principals WHERE kind <> 'executor';

DROP TABLE principals;
ALTER TABLE principals_old RENAME TO principals;

PRAGMA foreign_keys = ON;
