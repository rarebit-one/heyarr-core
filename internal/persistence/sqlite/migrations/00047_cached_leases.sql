-- +goose Up
-- Cached access leases: the CONSUMER half of §54 / #285 (ADR-0040, ADR-0048).
--
-- 00035 (access_leases) is the ISSUER's record of what THIS peer minted. This
-- table is the other side: the signed lease tokens a peer FETCHED from its
-- siblings ahead of an outage, so that when the controller is gone it can decide
-- for whom it may serve bytes (§53) reaching nobody — the token verifies against
-- a sibling this peer already pinned (ADR-0012), exactly as Store.Honour does.
--
-- Only the TOKEN is stored, because a token is opaque and self-authorising: the
-- consumer cannot read its fields without the issuer's key, and it does not need
-- to — grant.Verify checks principal, resource, capability and expiry against the
-- request at honour time. The columns here are just enough to mirror and refresh
-- the cache; they are not the authority.
--
-- The cache is a MIRROR of each sibling's active set, keyed by source_peer so a
-- re-fetch from one sibling (which overwrites its rows) drops whatever that
-- issuer let lapse without touching another sibling's cache and without this peer
-- pruning by expiry independently. During an outage no refresh happens and
-- grant.Verify's own clock check refuses an expired token — the degraded-refusal
-- property (§53), by the peer's own clock, controller nowhere in the loop.
CREATE TABLE cached_leases (
    source_peer TEXT NOT NULL,   -- the sibling this token was fetched from (rendered peer key)
    token       TEXT NOT NULL,   -- the signed grant token (grant.Sign output); opaque + self-authorising
    fetched_at  TEXT NOT NULL,   -- when this peer last cached it (RFC3339; provenance, not authority)
    PRIMARY KEY (source_peer, token)
) STRICT;

-- +goose Down
DROP TABLE cached_leases;
