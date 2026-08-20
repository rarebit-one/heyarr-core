-- +goose Up
-- Consumption sessions: watching, listening and reading, in one table
-- (§67, ADR-0024, M2-06).
--
-- 00008 is reserved for probe results (M2-04) and 00009 is devices (M2-05).
-- The milestone's numbers were claimed up front because two M1 branches once
-- took non-contiguous ones and broke two rollback tests.
--
-- # Why one table and not three
--
-- The tempting split is watch/listen/read, because their progress units differ:
-- a media timestamp, a track offset, a page or an EPUB CFI. That difference is
-- ONE column. The state machine, the resume query, the event vocabulary and
-- the eventual sync protocol are identical across all three, and building three
-- of each to accommodate one column is how a system ends up with three sync
-- protocols to keep consistent. ADR-0024 records the decision and what would
-- make us revisit it.
--
-- # What this is not
--
-- Not personal state. Ratings, annotations and the encrypted CRDT plane are
-- §37–47 and Milestone 9. A consumption session is CONTROL-PLANE state about a
-- playback — which device, which asset, what state, where it reached — and it
-- must not start accumulating things that belong in an encrypted space, or M9
-- becomes a migration out of here rather than an addition alongside it.

CREATE TABLE consumption_sessions (
    id        TEXT PRIMARY KEY,           -- UUIDv7 (ADR-0017)

    -- The asset being consumed. ON DELETE CASCADE because a session for an
    -- asset that no longer exists is not history worth keeping — it is a
    -- dangling reference that every read path would have to special-case.
    asset_id  TEXT NOT NULL REFERENCES assets (id) ON DELETE CASCADE,

    -- The device it is playing on. RESTRICT rather than CASCADE: a device is
    -- re-registered rather than deleted (M2-05), so a delete here is either an
    -- operator or a bug, and losing playback history to either is worse than
    -- refusing.
    device_id TEXT NOT NULL REFERENCES devices (id) ON DELETE RESTRICT,

    verb  TEXT NOT NULL CHECK (verb IN ('watch', 'listen', 'read')),
    state TEXT NOT NULL CHECK (state IN ('created', 'playing', 'paused', 'stopped', 'completed')),

    -- Progress is a LOCATOR and a UNIT, never a float. A page number is not a
    -- number of seconds, and storing both as "position" makes every reader
    -- guess which it has. Empty locator means nothing has been recorded, which
    -- is different from position zero.
    progress_locator TEXT NOT NULL DEFAULT '',
    progress_unit    TEXT NOT NULL DEFAULT '' CHECK (progress_unit IN ('', 'seconds', 'page', 'cfi')),

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    -- NULL until the session first plays, so a session created and abandoned is
    -- distinguishable from one that played and stopped. Without it the history
    -- cannot tell "nobody watched this" from "someone watched two minutes".
    started_at TEXT,
    -- NULL until terminal.
    ended_at   TEXT,

    -- Table constraints. They come after every column because SQLite requires
    -- it, and the two here are both about relationships BETWEEN columns, which
    -- is the only kind a column constraint cannot express.
    --
    -- A unit with no locator is a reader that knows how to measure and has not
    -- measured; a locator with no unit is a number nobody can interpret. Only
    -- the second is nonsense, so only the second is refused.
    CHECK (progress_locator = '' OR progress_unit <> ''),

    -- Terminal states have an end; non-terminal ones do not. The invariant is
    -- the database's job, not the caller's.
    CHECK (
        (state IN ('stopped', 'completed') AND ended_at IS NOT NULL)
        OR (state IN ('created', 'playing', 'paused') AND ended_at IS NULL)
    )
) STRICT;

-- "Continue watching" is the query this index exists for: the most recent
-- unfinished session, newest first. It is partial because the interesting set
-- is the small one — a library that has been used for a year has thousands of
-- completed sessions and a handful of open ones, and an index over both makes
-- the common query pay for the history.
CREATE INDEX consumption_sessions_resumable
    ON consumption_sessions (updated_at DESC, id)
    WHERE state IN ('created', 'playing', 'paused');

CREATE INDEX consumption_sessions_by_asset ON consumption_sessions (asset_id, updated_at DESC);
CREATE INDEX consumption_sessions_by_device ON consumption_sessions (device_id, updated_at DESC);

-- +goose Down
DROP INDEX consumption_sessions_by_device;
DROP INDEX consumption_sessions_by_asset;
DROP INDEX consumption_sessions_resumable;
DROP TABLE consumption_sessions;
