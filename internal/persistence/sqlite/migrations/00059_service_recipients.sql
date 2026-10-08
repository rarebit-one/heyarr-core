-- +goose Up
-- Service recipients (ADR-0104): an executor's X25519 public key, registered
-- as a wrap recipient tied to an executor principal.
--
-- Enrol-before-wrap (ADR-0049) admits a space-key wrap only for a pinned key:
-- an enrolled device's encryption key or a user's recovery key. An executor is
-- neither, and must never be enrolled as a member device. This table is the
-- second, narrower way a key becomes pinned. A management-authorised device
-- registers it explicitly, after the operator has compared the fingerprint the
-- executor's host displayed. Even then it is a valid wrap target only on a space
-- where its principal holds an active grant, so a wrap never runs ahead of the
-- owner's consent.
--
-- A registration is this node's fact, like a grant, and does not replicate.
-- A removed registration keeps its row; revoked_at says when the key stopped
-- being a recipient, and the event log names the device that removed it.

CREATE TABLE service_recipients (
    id                      TEXT PRIMARY KEY,

    -- The executor principal the key acts for. Its grants decide where the key
    -- may be wrapped; its restricted token is shown this key's wraps and no
    -- other recipient's.
    principal_id            TEXT NOT NULL REFERENCES principals (id) ON DELETE CASCADE,

    -- "x25519:<hex>", exactly as wrapped_keys.recipient spells it.
    recipient               TEXT NOT NULL,

    -- An operator's note ("home executor"), and the short fingerprint the
    -- operator compared against the one the executor's host displayed.
    label                   TEXT NOT NULL DEFAULT '',
    fingerprint             TEXT NOT NULL,

    -- Who registered it: the user principal and the device key whose
    -- management-authorised credential made the request (ADR-0065).
    registered_by_principal TEXT NOT NULL,
    registered_by_device    TEXT NOT NULL,
    created_at              TEXT NOT NULL,
    revoked_at              TEXT
) STRICT;

-- One live registration per key. A removed key may be registered again later,
-- for the same or another executor.
CREATE UNIQUE INDEX service_recipients_active_key
    ON service_recipients (recipient) WHERE revoked_at IS NULL;

CREATE INDEX service_recipients_by_principal ON service_recipients (principal_id);

-- +goose Down
DROP TABLE service_recipients;
