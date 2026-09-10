-- +goose Up
-- When to next ask an enrich provider about a held music/book Work (ADR-0087).
--
-- Enrichment is a JOB over a held Work, not a want (ADR-0087 §6): there is no
-- desired_items row, no acquisition_state, nothing that paces how often a Work
-- with no provider match is re-asked. This table is that pacing — the sibling of
-- subtitle_fetch_schedule for the enrich beat. Without it a Work no authority
-- knows (a self-published book Open Library has never seen) would be re-asked
-- every tick forever, and a Work whose cover just landed would be re-asked in the
-- window before the next pass reads its new asset.
--
-- One row per Work, keyed by the Work; CASCADE because a schedule for a Work
-- nobody holds any more is a dangling row the pass would read forever.
CREATE TABLE enrich_schedule (
    work_id TEXT PRIMARY KEY
        REFERENCES works (id) ON DELETE CASCADE,

    -- Consecutive passes that did not fully enrich the Work. The exponent in the
    -- backoff, not an error counter: a Work no authority knows is the ordinary
    -- case, so a fruitless pass backs it off toward a ceiling rather than failing
    -- it (the follow poll's stance, ADR-0057) — capped, never abandoned.
    fruitless INTEGER NOT NULL DEFAULT 0 CHECK (fruitless >= 0),

    -- Fixed-width UTC timestamps, for the lexicographic comparison the due query
    -- makes (see sortable in the catalog).
    last_enriched_at TEXT NOT NULL DEFAULT '',
    next_enrich_at   TEXT NOT NULL,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE INDEX enrich_due ON enrich_schedule (next_enrich_at);

-- +goose Down
DROP TABLE enrich_schedule;
