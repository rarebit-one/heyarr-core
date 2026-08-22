-- +goose Up
-- The search scheduler's bookkeeping: when each want is next due to be
-- searched, and how many consecutive searches have changed nothing (#130).
--
-- 00019 is peer identity; 00020-00024 are claimed by other in-flight work.
--
-- # Why a table at all, rather than deriving it
--
-- The tempting version derives everything: "search anything idle whose last
-- search event is older than an hour", read from the event log. It is
-- appealing because it stores nothing, and it fails on the one property this
-- feature exists for.
--
-- Backoff needs the count of CONSECUTIVE fruitless searches, and a derivation
-- has to reconstruct that by walking the event log backwards on every pass,
-- for every want, deciding as it goes which events count as "the same
-- fruitless streak". That is a scan whose cost grows with history rather than
-- with the library, to answer a question with exactly one integer in it.
--
-- It also loses the thing that makes a want's schedule EXPLAINABLE. `SELECT
-- next_search_at FROM search_schedule` answers "when will Heyarr look for this
-- again", which is the question #130 says the system could not answer about
-- itself — a MISSING want with nothing in flight was indistinguishable from
-- one being worked on.
--
-- # It is bookkeeping, not state
--
-- Losing this table costs one search per want, immediately: a want with no row
-- is due now. So it is deliberately NOT part of the acquisition state machine
-- in 00014, which is four load-bearing facts (ADR-0027) that must never be
-- reconstructible by guessing. Keeping the two apart also means the scheduler
-- can be retuned, or replaced, without a migration to acquisition_state.

CREATE TABLE search_schedule (
    -- One row per want; the want is the identity. CASCADE because a schedule
    -- for a want nobody holds any more is not history worth keeping, it is a
    -- dangling row the pass would read forever.
    desired_item_id TEXT PRIMARY KEY
        REFERENCES desired_items (id) ON DELETE CASCADE,

    -- WHICH schedule this row is counting against.
    --
    -- Stored rather than re-derived, because it is what makes the streak
    -- honest: a want that was MISSING for a week and has just become satisfied
    -- is now asking a different question, and carrying its fruitless count
    -- across would start its first upgrade search two weeks late. A change of
    -- schedule resets the count, and this column is how the change is seen.
    schedule TEXT NOT NULL CHECK (schedule IN ('missing', 'upgrade')),

    -- Consecutive searches on this schedule that left the want where it was.
    --
    -- Named for what it MEANS rather than for what it counts. A search that
    -- finds nothing is not a failure — a release that does not exist yet is
    -- the common case (§60) — so this is not an error counter and must never
    -- be read as one. It is the exponent in the backoff and nothing else.
    fruitless INTEGER NOT NULL DEFAULT 0 CHECK (fruitless >= 0),

    -- When the scheduler last enqueued a search for this want, and when it
    -- will next be due. Both are stored fixed-width — see the note on
    -- sortableTimestamp in the catalog: RFC3339Nano TRIMS trailing zeros from
    -- the fractional second, and SQLite compares TEXT lexicographically, so
    -- the two together make '…:00Z' sort AFTER '…:00.000000001Z'. The due
    -- query is exactly that comparison.
    last_searched_at TEXT NOT NULL,
    next_search_at   TEXT NOT NULL,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- The pass asks one question — "what is due?" — on a beat, forever. Without
-- this it is a table scan over every want in the library every time.
CREATE INDEX search_schedule_due ON search_schedule (next_search_at);

-- +goose Down
DROP INDEX search_schedule_due;
DROP TABLE search_schedule;
