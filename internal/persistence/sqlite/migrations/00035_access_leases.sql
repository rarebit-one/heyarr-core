-- +goose Up
-- Access leases: the Milestone 8 cross-site grant, cached (§54, ADR-0048, #285).
--
-- A lease is a capability grant (internal/grant): a signed, expiring delegation
-- binding principal, resource, capabilities and expiry. It is ISSUED here by
-- this peer, SIGNED with this peer's Ed25519 identity (ADR-0012), and can be
-- verified at any peer that has this one in its membership — with no reach back
-- to the issuer, because the signature verifies against a key the honouring peer
-- already pinned. That is the whole point ADR-0038 could not otherwise give a
-- cross-site capability: the grant carries its own bound, so a peer honours it
-- during an outage and it self-limits by expiring (24h max, grant.MaxTTL).
--
-- The signed token is the authority; this table is the ISSUER's record of what
-- it minted, so a lease can be listed, revoked and cached to sibling peers ahead
-- of an outage. Honouring a PRESENTED token needs only the token and the pinned
-- issuer key, never this row — a peer that lost this table still honours a lease
-- it holds, which is exactly the degraded-read property (§53).
--
-- Revocation is a tombstone, not a delete: a revoked lease is honoured until it
-- expires (a stated consequence, ADR-0048 — revocation "takes effect when the
-- peers are back"), and keeping the row is how the issuer stops re-issuing and
-- how the revocation is auditable. The 24h cap is what makes that acceptable.

CREATE TABLE access_leases (
    id           TEXT PRIMARY KEY,           -- UUIDv7 (ADR-0017)

    -- What the lease binds. Stored as columns so the issuer can list and revoke
    -- by principal or resource; the AUTHORITY is the signed token, not these.
    principal    TEXT NOT NULL,              -- the user identity key the lease authorises
    resource     TEXT NOT NULL,
    capabilities TEXT NOT NULL,              -- comma-joined grant capabilities (read, ...)

    -- The issuing peer's public key, rendered. It is who a honouring peer checks
    -- the signature against, and it is this peer's own key for a lease it minted.
    issuer       TEXT NOT NULL,

    -- The signed grant token (grant.Sign output). This is the thing cached to
    -- peers and presented at honour time; the columns above are derived from it.
    token        TEXT NOT NULL,

    issued_at    TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    revoked_at   TEXT
) STRICT;

CREATE INDEX access_leases_by_principal ON access_leases (principal, resource);

-- +goose Down
DROP INDEX access_leases_by_principal;
DROP TABLE access_leases;
