-- +goose Up
-- Membership ops: the op log an identity's device set is EVALUATED from
-- (ADR-0068; voidbind-go ADR-0007).
--
-- # Why 00022 and not 00043
--
-- 00022 is the lowest reserved-and-unspent number (see 00021's note: a gap
-- "reads as a DELETED migration", so the sequence is kept dense by filling the
-- lowest free number rather than appending). The migrator allows out-of-order
-- migrations for exactly this policy (migrate.go, goose.WithAllowMissing), so a
-- database already at 42 applies this one on its next start.
--
-- # What it is
--
-- Until now a device authenticated by ONE root-signed cert (00034): the user's
-- genesis key signed for the device key, and that was the whole of the
-- admission. ADR-0007 makes an identity a SET of device keys evolved by signed
-- ops — add / remove — that ANY current member may issue. A v1/v2 cert is read
-- as a genesis-signed add, so nothing already issued is reissued; but an add
-- signed by a phone, or a remove, is judged only against the ops in its causal
-- past (`prev`). A relying party therefore has to KEEP the ops it has seen —
-- this table — and evaluate the set (enrolment.Evaluate, pure) on every
-- authentication, merged with whatever the device presents beside its
-- credential (the Voidbind-Membership header).
--
-- The table is a G-set keyed by op hash: rows are only ever inserted, never
-- edited or deleted (a remove is a new row, not an update). Idempotent by
-- op_hash, so the same op learned twice — from two devices, or from a device
-- and POST /membership — is one row. It is server-opaque in the ADR-0007
-- sense: the node evaluates it but never signs into it. RevokeDevice (the
-- admin's tombstone on device_identities.revoked_at) is deliberately NOT an
-- op: the node is not a member of the identity and its word stays local.
--
-- device_identities (00034) stays as the MATERIALISED VIEW of this log: one
-- row per device the evaluation currently admits, `cert` holding the admitting
-- op token, `revoked_at` set when the evaluation removes the device or when
-- the admin tombstones it. Listing, naming, the encryption key (00036) and the
-- ADR-0065 write scope all keep reading that view; only authentication reads
-- this log.
CREATE TABLE membership_ops (
    -- "sha256:<hex>" of the raw token — the identity of an op, and what a later
    -- op cites in `prev`. Authoritative; the token is re-hashed on read never.
    op_hash     TEXT PRIMARY KEY,

    -- The identity (genesis key) the op belongs to. CASCADE: unpinning a user
    -- discards its log — with no pin there is nothing to evaluate against.
    user_id     TEXT NOT NULL REFERENCES user_identities (id) ON DELETE CASCADE,

    -- Denormalised from the signed payload, for listing and for the view
    -- reconciliation; the token is the source of truth and is re-verified on
    -- every evaluation, so a tampered row authorises nothing.
    dev         TEXT NOT NULL,               -- the device key the op adds or removes
    op          TEXT NOT NULL,               -- 'add' | 'remove'
    signer      TEXT NOT NULL,               -- `by`: a member's device key, or the genesis key
    prev        TEXT NOT NULL,               -- JSON array of op_hash the signer cited
    iat         TEXT NOT NULL,               -- issued-at, RFC3339 UTC (ADR-0017)

    -- The op token exactly as presented. It is what GET /membership returns
    -- and what Evaluate re-verifies.
    token       TEXT NOT NULL,

    received_at TEXT NOT NULL                -- when THIS node first recorded it
) STRICT;

CREATE INDEX membership_ops_by_user ON membership_ops (user_id, iat);

-- +goose Down
DROP INDEX membership_ops_by_user;
DROP TABLE membership_ops;
