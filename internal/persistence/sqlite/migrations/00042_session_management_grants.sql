-- +goose Up
-- Follow-management grants (ADR-0061, M12) — the interim, operator-issued
-- authorization that lifts a web-login session (ADR-0053) from the read floor
-- to write.
--
-- # Why a table, and why keyed on the approving device
--
-- A browser or television logs in by QR: it holds no device cert, and the
-- session the broker mints for it is read-only (ADR-0053), so POST/DELETE
-- /followed-sources 403 from such a client. Device-cert enrolment (ADR-0048) is
-- the eventual convergence that gives the surface its own authenticated identity;
-- until the voidbind-client artifact lands, this is the interim, non-gated path.
--
-- A web-login session carries exactly one identity: the APPROVING device's key
-- (the phone that signed the challenge), surfaced as the session principal's
-- device_key. That is the only stable thing an operator can authorise, so a grant
-- is keyed on it: the operator names a trusted personal device's key, and every
-- web-login session that device approves then carries `write`. The default —
-- no row — is read-only, so a shared surface (a TV) stays read-only until its
-- approving device is explicitly granted. The sharp edge is honest and recorded
-- in ADR-0061: the grant follows the approver, not the surface, so a TV logged in
-- BY a granted device would inherit write; the convergence on ADR-0048 binds
-- authority to the surface instead.
--
-- Single-writer control-plane state, like tokens and device identities
-- (invariant 5). The grant is an authorization fact, not content, so it lives
-- here and its transitions emit identity.device.management_* events (invariant 7).
CREATE TABLE session_management_grants (
    -- The approving device key this grant authorises, in rendered
    -- "ed25519:<hex>" form — the same value a web-login session principal
    -- carries as its device_key (ADR-0053). One grant per device key.
    device_key TEXT PRIMARY KEY,

    -- A free-text operator note ("the operator's phone"), never interpreted. Empty is
    -- allowed and is the default when a grant is issued without a reason.
    reason TEXT NOT NULL DEFAULT '',

    -- When the grant was issued (RFC3339, UTC — ADR-0017).
    granted_at TEXT NOT NULL
) STRICT;

-- +goose Down
DROP TABLE session_management_grants;
