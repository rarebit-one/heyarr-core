-- +goose Up
-- The durable job queue (spec §75, ADR-0008).
--
-- One job system for every content type and every plane. §61 names
-- "content-specific job systems" as an *arr constraint to avoid, and every
-- later milestone is "register a new job type" against this table rather than
-- "build another scheduler".

CREATE TABLE jobs (
    id       TEXT PRIMARY KEY,              -- UUIDv7 (ADR-0017)
    type     TEXT NOT NULL,
    payload  TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload)),

    state    TEXT NOT NULL DEFAULT 'pending'
             CHECK (state IN ('pending', 'leased', 'succeeded', 'failed', 'dead')),
    priority INTEGER NOT NULL DEFAULT 100,  -- lower runs first

    -- Idempotency. Enqueueing the same logical work twice while the first is
    -- still live must yield one row, not two (ADR-0008).
    dedupe_key TEXT,

    -- Capability routing (§75). A worker claims only what it can run: a box
    -- with no GPU should never lease a transcode.
    required_capability TEXT NOT NULL DEFAULT '',

    run_after  TEXT NOT NULL,               -- earliest time this may be claimed
    attempts   INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 5 CHECK (max_attempts > 0),

    -- Lease. A worker holds a job for a bounded time and renews by heartbeat;
    -- if it dies, the lease expires and the reaper returns the job to pending.
    lease_owner      TEXT,
    lease_expires_at TEXT,

    last_error  TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    finished_at TEXT,

    -- A leased job must have a lease; an unleased one must not. Without this,
    -- a bug that clears state without clearing lease_owner leaves a job that
    -- looks claimable and looks claimed at the same time.
    CHECK (
        (state = 'leased' AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (state <> 'leased' AND lease_owner IS NULL AND lease_expires_at IS NULL)
    )
) STRICT;

-- The claim query's index: pending work, cheapest first, that is due now.
CREATE INDEX jobs_claimable ON jobs (state, priority, run_after)
    WHERE state = 'pending';

-- The reaper's index: leases that have expired.
CREATE INDEX jobs_expiring ON jobs (lease_expires_at) WHERE state = 'leased';

-- Idempotency applies only to LIVE jobs. A completed job must not block the
-- same work being enqueued again later — otherwise a library could be scanned
-- exactly once, ever.
CREATE UNIQUE INDEX jobs_dedupe ON jobs (dedupe_key)
    WHERE dedupe_key IS NOT NULL AND state IN ('pending', 'leased');

CREATE INDEX jobs_by_type_state ON jobs (type, state);

-- +goose Down
DROP TABLE jobs;
