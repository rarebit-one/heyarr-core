-- +goose Up
-- Scan progress, made durable (M1-12).
--
-- A scan of a 4 TB library is minutes to hours of work that must survive the
-- process being restarted halfway through. Two things make that true, and this
-- migration is the second of them:
--
--   * scanned_files (00002) is the fingerprint cache. It is written AS THE SCAN
--     GOES, not at the end, so a scan that is cancelled mid-flight resumes
--     without re-reading what already landed.
--   * scan_runs is the observable half. Without a row per run, "is it still
--     going, and how far in?" is answerable only by tailing a log, and
--     system.scan.progress events (§76) have nothing durable to be a view of.

CREATE TABLE scan_runs (
    id      TEXT PRIMARY KEY,                       -- UUIDv7 (ADR-0017)
    root_id TEXT NOT NULL REFERENCES library_roots (id) ON DELETE CASCADE,

    -- 'cancelled' is a first-class outcome, not a failure: a worker draining on
    -- SIGTERM stops its scan deliberately, and recording that as 'failed' would
    -- make an ordinary restart look like an incident.
    state TEXT NOT NULL DEFAULT 'running'
          CHECK (state IN ('running', 'completed', 'cancelled', 'failed')),

    files_seen      INTEGER NOT NULL DEFAULT 0 CHECK (files_seen >= 0),
    files_enqueued  INTEGER NOT NULL DEFAULT 0 CHECK (files_enqueued >= 0),
    files_unchanged INTEGER NOT NULL DEFAULT 0 CHECK (files_unchanged >= 0),
    files_skipped   INTEGER NOT NULL DEFAULT 0 CHECK (files_skipped >= 0),
    files_missing   INTEGER NOT NULL DEFAULT 0 CHECK (files_missing >= 0),
    -- Errors are counted rather than fatal: a library with one bad mount must
    -- still scan (M1-12).
    errors     INTEGER NOT NULL DEFAULT 0 CHECK (errors >= 0),
    bytes_seen INTEGER NOT NULL DEFAULT 0 CHECK (bytes_seen >= 0),

    started_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    finished_at TEXT,
    last_error  TEXT,

    -- A finished run has a finish time and a running one does not. Without
    -- this, a bug that sets state without stamping the clock leaves a run that
    -- reads as both done and in flight.
    CHECK (
        (state = 'running' AND finished_at IS NULL)
        OR (state <> 'running' AND finished_at IS NOT NULL)
    )
) STRICT;

-- "What happened to this root, most recent first" is the only question asked of
-- this table, by the scan progress view and by an operator.
CREATE INDEX scan_runs_by_root ON scan_runs (root_id, started_at DESC);

-- At most one run per root may be in flight. Two concurrent scans of the same
-- root would interleave their fingerprint writes and each compute a different,
-- wrong set of vanished paths — so the database refuses it, rather than relying
-- on the job queue's dedupe key alone (which only covers jobs, not a scan
-- started any other way).
CREATE UNIQUE INDEX scan_runs_one_live_per_root ON scan_runs (root_id)
    WHERE state = 'running';

-- +goose Down
DROP INDEX scan_runs_one_live_per_root;
DROP INDEX scan_runs_by_root;
DROP TABLE scan_runs;
