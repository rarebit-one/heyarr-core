-- +goose Up
-- What each worker in the fleet has PROVEN it can do, and until when (§6, §75,
-- ADR-0023, ADR-0039, M5-112).
--
-- # Why a table at all
--
-- Capabilities have existed since M1-05 as an in-memory `Config.Capabilities`
-- on a running worker, passed as an argument to `jobs.Claim`. That is enough to
-- ROUTE work and it is not enough to ANSWER anything: ADR-0023 says so in its
-- own consequences — "the peer model has a mode per peer, not a capability
-- list, so there is nowhere to read a fleet-wide answer from yet". A worker's
-- abilities were observable only by watching which jobs it took.
--
-- This is that missing place. `/api/v1/capabilities` reads it, and the question
-- it answers — "which nodes can encode HEVC at all" — has no other source.
--
-- # One row per (worker, capability), not a serialised set
--
-- The read path is "which workers hold X", and a JSON array in a column turns
-- that into a full scan plus a parse in Go. A row per capability makes it an
-- index seek, and it is also what makes NARROWING a DELETE the database can be
-- asked about rather than a diff somebody has to compute correctly.
--
-- # expires_at is not decoration, and it is stored rather than derived
--
-- A worker that dies must stop advertising. It cannot delete its own row on the
-- way out — a power cut, an OOM kill and a severed network partition are
-- exactly the deaths that skip the tidy-up path, and they are the deaths that
-- matter. So every row carries its own expiry and every reader filters on it.
--
-- Stored per row rather than computed from proved_at plus a constant because
-- the TTL belongs to the ADVERTISER: a worker that re-verifies every five
-- minutes and one that re-verifies every hour must not share a deadline chosen
-- by whoever wrote the query.
--
-- # source says how the claim was established
--
--   binary   the tool resolved at startup and has not been re-resolved since.
--            ADR-0023's stance, unchanged: installing ffmpeg under a running
--            Heyarr is not a supported flow.
--   probe    the capability was EXERCISED — a real encode of a handful of
--            frames to a null sink — and the process exited successfully.
--            ADR-0039's whole point: ffmpeg will LIST a hardware encoder the
--            silicon cannot run, and nothing it prints distinguishes the two.
--   service  an external network service reported itself healthy (ADR-0025).
--
-- It is a column rather than a comment because the re-verification cadence
-- differs by source and an operator reading a stale-looking row needs to know
-- which rule applies to it.
--
-- # No foreign key to peers, deliberately
--
-- Same shape of reason as durability_evidence (00028): revocation is deletion
-- (ADR-0012), and a peer row disappearing must not silently erase the record of
-- what its workers could do while they were running. The row expires on its own
-- schedule instead. peer_name and endpoint are denormalised for the same
-- reason: an advertisement naming an id nobody can resolve is not readable.
CREATE TABLE worker_capabilities (
    -- The advertising worker. This is the job queue's lease owner, which is
    -- unique per PROCESS (see internal/worker/worker.go owner()) — so a
    -- restarted worker advertises under a new id and its predecessor's rows
    -- expire rather than being inherited. That is the honest behaviour: the
    -- old process's proof was about a process that no longer exists.
    worker_id  TEXT NOT NULL,
    -- The exact capability string, as matched by jobs.Claim's
    -- `required_capability IN (...)`. Dotted and hierarchical — `ffmpeg`,
    -- `ffmpeg.encoder.hevc`, `ffmpeg.encoder.av1.vaapi`. Matching stays EXACT:
    -- `ffmpeg` is a prefix of `ffmpeg.encoder.hevc` and a LIKE would route AV1
    -- work to a node that merely has the binary.
    capability TEXT NOT NULL,
    peer_id    TEXT NOT NULL DEFAULT '',
    peer_name  TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL CHECK (source IN ('binary', 'probe', 'service')),
    -- When this was last established. For a probe that is when the encode ran,
    -- not when the row was written, so a re-advertisement that reuses a cached
    -- proof cannot silently look fresh.
    proved_at  TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    -- A few words for an operator: which encoder, which accelerator, what the
    -- probe saw. Never a command line and never stderr — this is read over an
    -- API and a path is more than it needs.
    detail     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (worker_id, capability)
) STRICT;

-- "Which nodes hold capability X, right now" — the only question asked of this
-- table, and it always arrives as a capability plus a moment.
CREATE INDEX worker_capabilities_by_capability
    ON worker_capabilities (capability, expires_at);

-- +goose Down
DROP INDEX worker_capabilities_by_capability;
DROP TABLE worker_capabilities;
