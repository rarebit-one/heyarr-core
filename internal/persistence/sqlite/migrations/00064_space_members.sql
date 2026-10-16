-- +goose Up
-- Shared-space membership (ADR-0108, §47): which users a space is shared with.
--
-- The space is the unit of sharing. A member's enrolled devices may each hold
-- a wrapped copy of the space key; a user who is not a member may not be given
-- one. This table is the membership record enrol-before-wrap (ADR-0049) now
-- consults in addition to the device pin: a wrap for an enrolled device whose
-- user is not a current member of a `personal`, `shared` or `research` space is
-- refused. A `family` space is every household member's by definition, so its
-- rows are kept for listing and audit and never narrow the wrap gate.
--
-- Who changes membership follows ADR-0104's owner rule: the recorded owner's
-- device for a space that has one, any management-authorised device for a
-- legacy space with none, and any such device for a `family` space.
--
-- A removal is a tombstone, not a delete: removed_at says when the member was
-- removed, and while it is set no new wrap for that user's devices is admitted
-- — including on a rotation, which must name them in `revoke`. The tombstone
-- is what the owner's device reads to know which recipients the rotation must
-- leave out. A re-add clears it.
--
-- Deliberately no foreign key on principal_id (as encrypted_spaces.owner): a
-- deleted member's row keeps saying they were a member and when they stopped.
-- Membership is a replicated fact (ADR-0108); the replication of this table is
-- not yet built, and a principal id is this node's — the portable identity is
-- the user's pinned key, joined from user_identities when that path lands.
CREATE TABLE space_members (
    space_id           TEXT NOT NULL REFERENCES encrypted_spaces (id) ON DELETE CASCADE,
    principal_id       TEXT NOT NULL,
    role               TEXT NOT NULL CHECK (role IN ('owner', 'member')),
    added_at           TEXT NOT NULL,
    -- Who added them: the user principal and the device key whose
    -- management-authorised credential made the request (ADR-0065). Empty
    -- device for the owner row a space creation records.
    added_by_principal TEXT NOT NULL,
    added_by_device    TEXT NOT NULL DEFAULT '',
    removed_at         TEXT,
    PRIMARY KEY (space_id, principal_id)
) STRICT;

CREATE INDEX space_members_by_principal ON space_members (principal_id);

-- +goose Down
DROP TABLE space_members;
