-- +goose Up
-- Device identity: the Milestone 8 authentication tables (§40, ADR-0048, ADR-0032).
--
-- These are NOT the capability-profile `devices` table (00009). That one records
-- what a television can play and holds no key — "Heyarr has no way to interrogate
-- a television". These record cryptographic identity: a user's pinned public key,
-- and the device keys that user has vouched for. Two different things called a
-- "device", kept apart on purpose: a nullable public_key bolted onto the profile
-- table (whose rows are mostly TVs) is exactly the antipattern 00009's own comment
-- warns about, so identity gets its own tables rather than retrofitting that one.
--
-- A user identity is a principal (00002) of kind 'user' with a pinned Ed25519
-- public key — the same trust-root shape ADR-0012 uses for peers: the key IS the
-- identity, pinning it is enrolment, and it is what a cert and a grant are
-- verified against (ADR-0048, internal/enrolment, internal/grant). Revoking a
-- user is deleting the pin, exactly as peer revocation deletes the membership row.
-- The 'user' kind has been legal in 00002 since M1 and nothing wrote it until now.

CREATE TABLE user_identities (
    id           TEXT PRIMARY KEY,           -- UUIDv7 (ADR-0017)
    principal_id TEXT NOT NULL UNIQUE REFERENCES principals (id) ON DELETE CASCADE,

    -- The pinned public key, rendered "ed25519:<hex>" (identity.FormatPublicKey).
    -- Stored as text rather than the raw bytes peers use, because this boundary
    -- already holds the rendered form as its value (a cert's issuer, a grant's
    -- issuer) and there is no equivalent of the peer surface's hot per-connection
    -- byte comparison to optimise for. UNIQUE: one key is one identity.
    public_key   TEXT NOT NULL UNIQUE,

    enrolled_at  TEXT NOT NULL
) STRICT;

CREATE TABLE device_identities (
    id          TEXT PRIMARY KEY,            -- UUIDv7

    -- The user this device speaks for. CASCADE: revoking a user takes its
    -- devices with it, because a device authenticates only as its user and a
    -- device with no user authenticates as nobody.
    user_id     TEXT NOT NULL REFERENCES user_identities (id) ON DELETE CASCADE,

    -- The device's Ed25519 public key, rendered form, UNIQUE across ALL users:
    -- a device key belongs to exactly one user, so a second user cannot enrol a
    -- key that is already someone's device.
    device_key  TEXT NOT NULL UNIQUE,

    name        TEXT NOT NULL DEFAULT '',

    -- The user-signed enrolment cert (an internal/enrolment Cert token). Stored
    -- so a device can be listed and revoked; authentication itself re-verifies
    -- the presented cert live against the pinned user key rather than trusting
    -- this copy, so a tampered row cannot authorise anything.
    cert        TEXT NOT NULL,

    enrolled_at TEXT NOT NULL,
    expires_at  TEXT NOT NULL,               -- the cert's expiry, for listing and pruning

    -- Revocation is a tombstone here, unlike peers (which delete the row): a
    -- revoked device key must stay KNOWN so a re-presented cert for it is refused
    -- rather than silently re-enrolled, and so the revocation is auditable. The
    -- CASCADE above still applies at the user level; this is per-device.
    revoked_at  TEXT
) STRICT;

CREATE INDEX device_identities_by_user ON device_identities (user_id);

-- +goose Down
DROP INDEX device_identities_by_user;
DROP TABLE device_identities;
DROP TABLE user_identities;
