-- +goose Up
-- When to next ask a provider for a want's subtitle (ADR-0085).
--
-- A subtitle want is direct-route: it is never handed to the indexer search
-- beat, so it has no search_schedule row and nothing paces how often the
-- subtitle provider is asked for it. This table is that pacing — the sibling of
-- search_schedule for the fetch beat. Without it a want the provider has no
-- subtitle for yet (an episode that just aired) would be re-asked every tick,
-- and a want whose subtitle just landed would be re-asked in the window before
-- the reconcile sweep marks it satisfied — both of which spend a provider's hard
-- daily quota on nothing.
--
-- One row per want, keyed by the want; CASCADE because a schedule for a want
-- nobody holds any more is a dangling row the pass would read forever.
CREATE TABLE subtitle_fetch_schedule (
    desired_item_id TEXT PRIMARY KEY
        REFERENCES desired_items (id) ON DELETE CASCADE,

    -- Consecutive fetches that did not find a subtitle. The exponent in the
    -- backoff, not an error counter: a subtitle that does not exist yet is the
    -- ordinary case, so a fruitless fetch backs the want off toward a ceiling
    -- rather than failing it (the follow poll's stance, ADR-0057).
    fruitless INTEGER NOT NULL DEFAULT 0 CHECK (fruitless >= 0),

    -- Fixed-width UTC timestamps, for the lexicographic comparison the due query
    -- makes (see sortableTimestamp in the catalog).
    last_fetched_at TEXT NOT NULL DEFAULT '',
    next_fetch_at   TEXT NOT NULL,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE INDEX subtitle_fetch_due ON subtitle_fetch_schedule (next_fetch_at);

-- +goose Down
DROP TABLE subtitle_fetch_schedule;
