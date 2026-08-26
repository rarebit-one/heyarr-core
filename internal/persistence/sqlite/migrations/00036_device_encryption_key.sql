-- +goose Up
-- The device encryption key: Milestone 9 pins, beside a device's signing key,
-- the X25519 ENCRYPTION key that space keys are wrapped for (§41, ADR-0049).
--
-- device_identities (00034) records a device's Ed25519 SIGNING key — what
-- authenticates it. §41 wraps a space key FOR a device, which is key agreement,
-- not signing, and needs a different primitive: an X25519 key. A v2 enrolment
-- cert (internal/enrolment) binds both keys under the one user signature, so a
-- wrapper on either peer can learn an enrolled device's encryption key from a
-- source the user signed, rather than from an unauthenticated claim.
--
-- It is a NOT NULL column defaulting to '' rather than a nullable one: a device
-- enrolled by a v1 cert (M8, signing key only) simply has no encryption key, and
-- '' says exactly that without a NULL to special-case. The empty default is also
-- what lets this ALTER run against existing device_identities rows without a
-- backfill — a pre-Milestone-9 device stays enrolled and authenticating, and is
-- not yet a wrap target until it re-enrols with a v2 cert.
ALTER TABLE device_identities ADD COLUMN encryption_key TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite before 3.35 could not DROP COLUMN; the pinned pure-Go driver supports
-- it, and the column is additive, so the down is a plain drop.
ALTER TABLE device_identities DROP COLUMN encryption_key;
