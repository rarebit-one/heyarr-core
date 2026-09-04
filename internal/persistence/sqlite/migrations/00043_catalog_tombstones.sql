-- +goose Up
-- Catalog tombstones: the first increment of the editorial catalog op-log the
-- two-site catalog converges by (ADR-0073, #449, Phase 1). Modelled directly on
-- membership_ops (00022): a G-set op log, plus a materialised view the rest of
-- the system reads.
--
-- # The gap this closes
--
-- Going active-active — peer-a at site-a and peer-b at site-b, either site
-- accepting writes — opens one plane that does not converge: the mutable
-- catalog rows. The byte-derivable spine (works get-or-created on `work_key`,
-- editions a scan re-derives, assets projected from CAS blobs) already
-- converges indirectly, because both peers hold the same blobs and run the same
-- identification. The EDITORIAL layer laid on top does not, and a logical
-- delete is its sharpest case: a delete at one site that does not reach the
-- other is worse than silent — the other site's next scan re-materialises
-- exactly what was removed (the "delete-then-rebuild" churn ADR-0071 refuses
-- inside one node, now across the pair).
--
-- # What it is
--
-- Two tables, the same shape as membership's log + view:
--
--   * catalog_ops — the op log, a G-set keyed by op hash. Rows are only ever
--     inserted, never edited or deleted; a restore is a NEW row, not an update.
--     Idempotent by op_hash, so an op learned twice (from a peer sync and from
--     a local write) is one row. The token is the source of truth and is
--     re-verified on every evaluation (internal/catalogop.Evaluate), so a
--     tampered denormalised column authorises nothing.
--
--   * work_tombstones — the MATERIALISED VIEW, one row per (content_type,
--     work_key) the evaluation currently tombstones, reconciled from the whole
--     log after every append (the RecordOps idiom). This is the row a scanner's
--     get-or-create consults to suppress a re-create, and the row a delete's
--     application removes the live `works` row against.
--
-- # Why the work's natural key, not works.id
--
-- works.id is a per-site UUIDv7 (ADR-0017): two peers mint different ids for
-- the same film, so the id cannot key a cross-site op. `work_key`
-- (`UNIQUE (content_type, work_key)` in 00002) is what a rescan already
-- converges on and is the natural cross-site-stable key — ADR-0073 open
-- question 1. Editions have no such key yet, so this increment is works only.
--
-- # Scope (ADR-0073 is only PROPOSED)
--
-- Tombstones for WORKS, remove-wins with a causal restore. follow_sources
-- (Phase 2) and the metadata/external_id overlay (Phase 3) are out. WHO signs a
-- catalog op (peer identity vs device write scope) is ADR-0073 open question 2;
-- `signer` records whatever key signed, and evaluation only checks the
-- signature is valid — the finer authority model lands with the accepted ADR.

CREATE TABLE catalog_ops (
    -- "sha256:<hex>" of the raw token — the identity of an op, and what a later
    -- op cites in `prev`. Authoritative; the token is re-hashed on read never.
    op_hash      TEXT PRIMARY KEY,

    -- Denormalised from the signed payload, for listing and for the view
    -- reconciliation; the token is re-verified on every evaluation, so a
    -- tampered row changes nothing.
    content_type TEXT NOT NULL,              -- the work's content type
    work_key     TEXT NOT NULL,              -- the work's normalised key (00002)
    op           TEXT NOT NULL               -- 'delete' | 'restore'
                 CHECK (op IN ('delete', 'restore')),
    signer       TEXT NOT NULL,              -- `by`: the peer key that signed (ADR-0012)
    prev         TEXT NOT NULL,              -- JSON array of op_hash the signer cited
    iat          TEXT NOT NULL,              -- issued-at, RFC3339 UTC (ADR-0017)

    -- The op token exactly as presented. It is what a peer sync returns and
    -- what Evaluate re-verifies.
    token        TEXT NOT NULL,

    received_at  TEXT NOT NULL               -- when THIS node first recorded it
) STRICT;

CREATE INDEX catalog_ops_by_target ON catalog_ops (content_type, work_key, iat);

-- The materialised view: the works the log currently tombstones. Present iff
-- the evaluation says the work is tombstoned; the row is DELETED when a later
-- causal restore lifts the tombstone, exactly as device_identities.revoked_at
-- is cleared by nothing but re-materialisation. Keyed by the natural key so a
-- scan's get-or-create can consult it without knowing any per-site id, and so a
-- tombstone outlives the `works` row it suppressed.
CREATE TABLE work_tombstones (
    content_type  TEXT NOT NULL,
    work_key      TEXT NOT NULL,
    -- The op the evaluation currently attributes the tombstone to (the earliest
    -- un-overridden delete's hash), for provenance an operator can read.
    op_hash       TEXT NOT NULL,
    tombstoned_at TEXT NOT NULL,             -- when this node materialised it
    PRIMARY KEY (content_type, work_key)
) STRICT;

-- +goose Down
DROP TABLE work_tombstones;
DROP INDEX catalog_ops_by_target;
DROP TABLE catalog_ops;
