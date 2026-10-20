-- +goose Up
-- What this node did with each acquire op it received (ADR-0119). An op is a
-- statement in a replicated G-set; applying it (creating the work and want,
-- recording the direct release, enqueueing the grab) is a local side effect
-- that must happen once per op and must survive a restart, so the outcome is a
-- row keyed by the op's hash. A retryable outcome (the op names an edition or
-- item this node has not materialised yet; no source here; the work is
-- tombstoned; the want is still fetching a sibling's earlier choice) is
-- re-evaluated on every pass; a final one is revisited only when its want
-- disappears or falls back to idle without content.
CREATE TABLE catalog_op_applications (
    op_hash         TEXT PRIMARY KEY REFERENCES catalog_ops (op_hash) ON DELETE CASCADE,
    desired_item_id TEXT,                    -- the want applied to, when one was ensured
    outcome         TEXT NOT NULL
                    CHECK (outcome IN ('applied', 'own', 'held', 'tombstoned', 'deferred',
                                       'scope_unresolved', 'no_site_source', 'profile_unknown')),
    detail          TEXT NOT NULL DEFAULT '',
    applied_at      TEXT NOT NULL            -- RFC3339 UTC (ADR-0017)
) STRICT;

-- The wants fan-out CREATED. They exist to fetch a sibling's exact release, so
-- this site's own scorer never searches for them: a search could select a
-- different release and the two sites would hold different bytes. A want that
-- existed before an op was applied to it is this site's own and stays on its
-- search schedule.
CREATE TABLE fan_out_wants (
    desired_item_id TEXT PRIMARY KEY REFERENCES desired_items (id) ON DELETE CASCADE,
    origin_site     TEXT NOT NULL,
    created_at      TEXT NOT NULL            -- RFC3339 UTC (ADR-0017)
) STRICT;

-- +goose Down
DROP TABLE fan_out_wants;
DROP TABLE catalog_op_applications;
