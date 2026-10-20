-- +goose Up
-- When to next ask a store to fetch a want's game (ADR-0120).
--
-- A game want is on the store route: it is never handed to the indexer search
-- beat, so it has no search_schedule row and nothing else paces how often the
-- store fetch is attempted. This table is that pacing, the sibling of
-- subtitle_fetch_schedule. Without it a game whose fetch keeps failing (an
-- expired store login, a package GOG will not vouch for) would be re-fetched on
-- every beat, tens of gigabytes at a time.
CREATE TABLE store_fetch_schedule (
    desired_item_id TEXT PRIMARY KEY
        REFERENCES desired_items (id) ON DELETE CASCADE,

    -- Consecutive attempts that did not leave the want satisfied. The exponent
    -- in the backoff, not an error counter.
    fruitless INTEGER NOT NULL DEFAULT 0 CHECK (fruitless >= 0),

    -- Fixed-width UTC timestamps, for the lexicographic comparison the due query
    -- makes (see sqlite.FormatTimestamp).
    last_attempted_at TEXT NOT NULL DEFAULT '',
    next_attempt_at   TEXT NOT NULL,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE INDEX store_fetch_due ON store_fetch_schedule (next_attempt_at);

-- +goose Down
DROP TABLE store_fetch_schedule;
