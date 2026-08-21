-- +goose Up
-- Releases that arrived as bad bytes, and must not be chosen again (§64, M3-13).
--
-- The last migration of Milestone 3. 00012 quality profiles, 00013 desired
-- items, 00014 acquisition state, 00015 providers, 00016 acquisitions, 00017
-- release candidates. This number was reserved when the milestone was planned
-- and is claimed here.
--
-- # Why this is a table and not a column on release_candidates
--
-- Because RecordSearch REPLACES a want's candidates wholesale (00017). A
-- search supersedes its predecessor, so a mark written onto a candidate row is
-- erased by the very next search — and the next search is precisely when the
-- mark needs to be read.
--
-- That is not a hypothetical ordering problem, it is the exact loop this table
-- exists to break:
--
--   search → select the best release → download → bytes do not verify
--     → mark it → search again → the mark was replaced → select the same
--     release → download it again → forever
--
-- A bad release is an infinite download unless the mark outlives the candidate
-- set, and the only way it outlives it is by not living in it.
--
-- # Why the key is (desired_item_id, provider, candidate_id)
--
-- Scoped to the WANT rather than global. The same release may be fine for one
-- want and wrong for another — a mislabelled file satisfies neither, but a
-- truncated download of one edition says nothing about a different edition
-- from the same indexer. A global blocklist would let one want's bad luck
-- silently narrow every other want's options, and nothing would say why.
--
-- The provider is in the key because a candidate id is only unique within the
-- indexer that issued it. Two indexers can and do use the same numeric ids.

CREATE TABLE blocked_releases (
    id TEXT PRIMARY KEY,                        -- UUIDv7 (ADR-0017)

    -- CASCADE: a blocklist entry for a want that no longer exists is a
    -- dangling row every read path would have to filter out.
    desired_item_id TEXT NOT NULL
        REFERENCES desired_items (id) ON DELETE CASCADE,

    -- Which indexer offered it, and its own id for the release. NOT a
    -- reference to release_candidates: that row is expected to disappear, and
    -- a foreign key to it would delete the mark along with it, which is the
    -- entire failure this table prevents.
    provider     TEXT NOT NULL,
    candidate_id TEXT NOT NULL,

    -- What the release was called when it was blocked. Provenance for a human
    -- reading the list — the candidate row is gone, so without this the entry
    -- is three opaque identifiers.
    title TEXT NOT NULL DEFAULT '',

    -- Why. Free text for an operator; the machine-readable half is `reason`.
    detail TEXT NOT NULL DEFAULT '',

    -- The class of failure, so a future policy can treat them differently
    -- without parsing prose. `verification_failed` is bytes that did not hash
    -- to what was claimed — the release was bad. `ingest_failed` is bytes that
    -- were fine and could not be brought under management, which is a local
    -- problem and arguably should NOT block the release; it is recorded
    -- distinctly so that decision can be made later on evidence rather than
    -- being baked in now by using one word for both.
    reason TEXT NOT NULL DEFAULT 'verification_failed'
        CHECK (reason IN ('verification_failed', 'ingest_failed', 'manual')),

    blocked_at TEXT NOT NULL,

    -- One entry per release per want. Blocking twice is the same statement,
    -- and the poll job that discovers a failure WILL be re-run (invariant 9).
    UNIQUE (desired_item_id, provider, candidate_id)
) STRICT;

-- The search job reads this per want on every pass, before it evaluates
-- anything.
CREATE INDEX blocked_releases_by_want ON blocked_releases (desired_item_id);

-- +goose Down
DROP INDEX blocked_releases_by_want;
DROP TABLE blocked_releases;
