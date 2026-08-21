-- +goose Up
-- The centralised provider registry's persisted half (§59, M3-07).
--
-- 00012 is quality profiles, 00013 desired items, 00014 acquisition state.
-- 00016 acquisitions, 00017 release candidates, 00018 held.
--
-- # What is here and what is NOT
--
-- Configuration lives in the config file, not in this table. An operator's
-- providers are declared alongside their libraries and their peer name, so a
-- node's whole identity is one reviewable document, and standing up a second
-- machine is copying a file rather than replaying a sequence of API calls.
--
-- What IS here is what configuration cannot hold: what the last health check
-- OBSERVED. That has to survive a restart — otherwise every restart reports
-- every provider as never-checked, and an operator watching a flapping indexer
-- loses the history exactly when they need it.
--
-- **No credential is ever stored here.** Not encrypted, not hashed, not
-- redacted-but-present. The plaintext lives in the operator's config file where
-- their other secrets already live, and putting a second copy in a database
-- that gets backed up to peers (§50) would be adding a way to leak it in
-- exchange for nothing. internal/providers.Secret keeps it out of logs and
-- responses; this table keeps it out of the backup stream.
--
-- # Keyed by name, which is the operator's word
--
-- Not a generated id. A provider's name is how it is referenced in routing, in
-- health and in a release candidate's Provider field, and an operator who
-- renames one in configuration means it is a different provider — the old
-- row's health is about a service that is no longer configured.
--
-- Rows for providers no longer in configuration are left alone rather than
-- deleted on sight: a provider commented out for an afternoon should come back
-- with its history, and a startup that silently deleted observations would make
-- "did that indexer ever work?" unanswerable.

CREATE TABLE provider_health (
    -- The operator's name for the provider, unique within an instance.
    name TEXT PRIMARY KEY CHECK (length(trim(name)) > 0),

    -- What this provider declared it can do, at the time of the check.
    --
    -- JSON for the same reason the device capability lists are: the length
    -- varies, nothing queries by an individual capability, and the registry is
    -- the thing that routes. json_type is asserted rather than only
    -- json_valid, because `"indexer"` is valid JSON and is not a list of
    -- capabilities — a scalar smuggled into a list column produces a provider
    -- that silently matches no routing at all.
    capabilities TEXT NOT NULL DEFAULT '[]'
        CHECK (json_type(capabilities) = 'array'),

    healthy INTEGER NOT NULL DEFAULT 0 CHECK (healthy IN (0, 1)),

    -- What was observed, in a few words, for an operator reading
    -- GET /api/v1/providers. Required in spirit: a status with no reason is one
    -- nobody can act on, and providers.Unhealthy defaults it rather than
    -- allowing an empty one through.
    detail TEXT NOT NULL DEFAULT '',

    -- The API version the service reported, empty when it did not answer or
    -- does not say one.
    --
    -- This is what replaces version PINNING for a service Heyarr does not
    -- install (ADR-0026). Not controlling the version does not mean ignoring
    -- it: recording what was seen is what turns "acquisitions stopped after I
    -- upgraded the NAS" into one request rather than an afternoon.
    version TEXT NOT NULL DEFAULT '',

    -- When the check ran. NULL means never — which is DISTINCT from unhealthy,
    -- and the distinction is the same one §56's satisfaction axes make:
    -- "nobody has looked" and "we looked and the answer is no" lead to
    -- different actions.
    checked_at TEXT,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- "What is broken" is the question an operator opens this table to ask, and it
-- is asked on every render of GET /api/v1/providers.
CREATE INDEX provider_health_unhealthy ON provider_health (healthy) WHERE healthy = 0;

-- +goose Down
DROP INDEX provider_health_unhealthy;
DROP TABLE provider_health;
