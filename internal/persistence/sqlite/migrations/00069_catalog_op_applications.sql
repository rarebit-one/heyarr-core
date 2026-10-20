-- +goose Up
-- What this node did with each acquire op it received (ADR-0119). An op is a
-- statement in a replicated G-set; applying it (creating the work and want,
-- recording the direct release, enqueueing the grab) is a local side effect
-- that must happen once per op and must survive a restart, so the outcome is a
-- row keyed by the op's hash. A retryable outcome (the op names an edition or
-- item this node has not materialised yet) is overwritten on the next pass; a
-- final one is left alone until a newer op supersedes it.
CREATE TABLE catalog_op_applications (
    op_hash         TEXT PRIMARY KEY REFERENCES catalog_ops (op_hash) ON DELETE CASCADE,
    desired_item_id TEXT,                    -- the want applied to, when one was ensured
    outcome         TEXT NOT NULL
                    CHECK (outcome IN ('applied', 'own', 'held', 'tombstoned',
                                       'scope_unresolved', 'no_site_source', 'profile_unknown')),
    detail          TEXT NOT NULL DEFAULT '',
    applied_at      TEXT NOT NULL            -- RFC3339 UTC (ADR-0017)
) STRICT;

-- +goose Down
DROP TABLE catalog_op_applications;
