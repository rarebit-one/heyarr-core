-- +goose Up
-- Acquisitions: the bridge between a want and an external transfer (§58, M3-10).
--
-- 00012 quality profiles, 00013 desired items, 00014 acquisition state,
-- 00015 providers. 00017 release candidates, 00018 held.
--
-- # Why this table exists at all
--
-- 00014 holds where a want IS in §64's pipeline. It does not hold what the
-- download client calls the thing that is moving, and without that the poll job
-- cannot answer the only question it has: which of these transfers is which
-- want.
--
-- # The identity is the download client's, and it is a hash
--
-- external_id is Transmission's infohash, never its torrent name and never its
-- numeric id.
--
--   * NAMES get renamed by the client, collide between releases, and are not
--     stable across a restart.
--   * Transmission's NUMERIC ids are assigned per session. A daemon rebuilt
--     from scratch — which happens, it is a container — reissues them from 1,
--     so a stored numeric id silently starts pointing at somebody else's
--     transfer.
--
-- An infohash is the same conclusion invariant 1 reaches for bytes, one level
-- up: identity is a hash, not a label a human reads.
--
-- # One in-flight acquisition per want
--
-- The UNIQUE on desired_item_id is what makes the poll job idempotent in the
-- way that matters. The job WILL be re-run (invariant 9), and re-running it
-- must not queue a second copy of a transfer already downloading — a duplicate
-- grab is the failure mode this whole design exists to prevent, and it presents
-- as bandwidth rather than as an error.
--
-- History is kept in the events log rather than here. A row per attempt would
-- make "what is in flight for this want" a query with an ORDER BY and a LIMIT,
-- which is the sort of thing that is right until somebody forgets the ORDER BY.

CREATE TABLE acquisitions (
    id TEXT PRIMARY KEY,                    -- UUIDv7 (ADR-0017)

    -- The want this is acquiring for. CASCADE because an acquisition for a
    -- want that no longer exists is not history worth keeping — it is a
    -- dangling reference every read path would have to special-case.
    --
    -- UNIQUE: one in-flight acquisition per want. See above.
    desired_item_id TEXT NOT NULL UNIQUE
        REFERENCES desired_items (id) ON DELETE CASCADE,

    -- Which configured provider is moving the bytes, by the operator's own
    -- name for it (§59). Not a foreign key: provider configuration lives in
    -- the config file, not in a table, and a provider commented out for an
    -- afternoon must not cascade away the acquisitions it started.
    provider TEXT NOT NULL CHECK (length(trim(provider)) > 0),

    -- The download client's identifier. An infohash for Transmission.
    external_id TEXT NOT NULL CHECK (length(trim(external_id)) > 0),

    -- What the client calls it, for a human reading a queue. Never used for
    -- identity — see above.
    external_name TEXT NOT NULL DEFAULT '',

    -- Where the client says the bytes are, in ITS namespace, once the transfer
    -- is complete.
    --
    -- Empty until then, and deliberately so: with incomplete-dir enabled a
    -- mid-transfer path does not exist, so storing one would hand ingest
    -- something that is not there and make a timing problem look like an
    -- ingest bug.
    remote_path TEXT NOT NULL DEFAULT '',

    -- The same path translated into one Heyarr can open, by the provider's
    -- path map. Stored alongside rather than instead of remote_path so that
    -- "the client said X, we looked at Y" is answerable when it goes wrong —
    -- which is the single most common operational failure in this class of
    -- software.
    local_path TEXT NOT NULL DEFAULT '',

    bytes_total INTEGER NOT NULL DEFAULT 0 CHECK (bytes_total >= 0),
    bytes_done  INTEGER NOT NULL DEFAULT 0 CHECK (bytes_done  >= 0),

    -- What is wrong, when something is.
    --
    -- Populated from the client's own error AND from its tracker statistics: a
    -- transfer whose tracker cannot be reached reports NOTHING at the top
    -- level, so a column filled only from errorString would be empty for the
    -- most common stall there is. See internal/downloads/stall.go.
    trouble TEXT NOT NULL DEFAULT '',

    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    -- When the client last told us anything about this. It is what makes "in
    -- flight since Tuesday and nobody noticed" findable, which updated_at
    -- cannot do because reconciliation touches that on its own schedule.
    last_seen_at  TEXT NOT NULL
) STRICT;

-- The poll job reconciles by (provider, external_id): it reads the client's
-- queue and has to find the row for each transfer. Without this that is a scan
-- per transfer per poll.
CREATE UNIQUE INDEX acquisitions_by_external
    ON acquisitions (provider, external_id);

-- "What is in flight" is the operational question, and it is asked on every
-- pass.
CREATE INDEX acquisitions_by_last_seen ON acquisitions (last_seen_at);

-- +goose Down
DROP INDEX acquisitions_by_last_seen;
DROP INDEX acquisitions_by_external;
DROP TABLE acquisitions;
