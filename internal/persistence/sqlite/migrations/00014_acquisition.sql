-- +goose Up
-- Acquisition state: where a want is in the pipeline, and §56's two answers
-- (§64, M3-03).
--
-- 00012 is quality profiles, 00013 desired items. 00015 providers, 00016
-- acquisitions, 00017 release candidates, with 00018 held.
--
-- # §64's twelve boxes are FOUR facts, not one ordinal
--
-- The spec draws a single column:
--
--   MISSING → SEARCHING → CANDIDATES_FOUND → SELECTED → QUEUED → DOWNLOADING
--   → VERIFYING → INGESTING → AVAILABLE → CONTENT_SATISFIED
--   → PLACEMENT_CONVERGING → FULLY_SATISFIED
--
-- Stored as one ordinal counting 0..11, CONTENT_SATISFIED and FULLY_SATISFIED
-- collapse into each other under the first simplification that looks tidy —
-- and that collapse is the one outcome the milestone epic explicitly names.
-- ADR-0027 records why, because the tidy version will look correct to the next
-- reader.
--
-- The collapse is not hypothetical. Obtaining a file and replicating it are
-- different work, done by different subsystems, at different times, and either
-- can regress without the other: a peer can go away long after the bytes
-- arrived. An ordinal cannot express "content is fine and placement went
-- backwards" without moving backwards through states that mean something else.
--
-- So four columns, and the §64 name is DERIVED and never stored:
--
--   phase      where the pipeline is
--   managed    whether Heyarr holds bytes for this want
--   content    do we hold bytes the quality profile accepts?     (§56)
--   placement  are those bytes on every Full Peer that should?   (§56)
--
-- # Why `managed` is not folded into the phase
--
-- Because a want can be acquiring an UPGRADE while already holding a perfectly
-- good copy. §64's MISSING and AVAILABLE both mean "nothing in flight" and
-- differ only in whether bytes are held — so with them as phases, a fruitless
-- upgrade search returns to MISSING and reports a good library as empty. The
-- first version of this migration had exactly that bug and the domain's own
-- upgrade test found it.

CREATE TABLE acquisition_state (
    -- One acquisition per want. The desired item IS the identity: an
    -- acquisition with no want is work nobody asked for, and two acquisitions
    -- for one want would race each other into the download client.
    desired_item_id TEXT PRIMARY KEY
        REFERENCES desired_items (id) ON DELETE CASCADE,

    -- The pipeline. There is no 'missing' or 'available' here — see above.
    phase TEXT NOT NULL DEFAULT 'idle' CHECK (phase IN (
        'idle', 'searching', 'candidates_found', 'selected',
        'queued', 'downloading', 'verifying', 'ingesting')),

    managed INTEGER NOT NULL DEFAULT 0 CHECK (managed IN (0, 1)),

    -- The two axes. Their value sets DIFFER and the difference is not an
    -- oversight: 'converging' is meaningless for content, because there is no
    -- partial file that half-satisfies a profile; and 'not_applicable' is
    -- meaningless for content, because content satisfaction is the whole
    -- question a DesiredItem asks.
    --
    -- 'unknown' is distinct from 'not_satisfied' on both axes. "Nobody has
    -- looked" and "we looked and the answer is no" lead to different actions,
    -- and collapsing them makes a fresh want indistinguishable from one just
    -- found wanting.
    content TEXT NOT NULL DEFAULT 'unknown'
        CHECK (content IN ('unknown', 'not_satisfied', 'satisfied')),

    -- 'not_applicable' exists for ADR-0020's linked assets, which have no blob
    -- and therefore nothing to replicate. Calling that 'satisfied' would be
    -- vacuously true and a lie by construction: FULLY_SATISFIED would mean
    -- "one copy, on one disk, with no integrity guarantee".
    placement TEXT NOT NULL DEFAULT 'unknown'
        CHECK (placement IN ('unknown', 'not_satisfied', 'converging',
                             'satisfied', 'not_applicable')),

    -- Why the last transition happened, when it was a failure. Free text for a
    -- human; the machine-readable reason lives on the release candidate that
    -- caused it (M3-12).
    detail TEXT NOT NULL DEFAULT '',

    -- When the pipeline last moved, as distinct from when the axes last did.
    -- A want stuck in 'downloading' for a week is the thing an operator needs
    -- to find, and updated_at alone cannot show it because reconciliation
    -- touches the axes on its own schedule.
    phase_entered_at TEXT NOT NULL,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,

    -- The floor under the domain's Validate. The validator runs in a process;
    -- a migration or a repair script does not go through it.
    --
    -- Content satisfaction is a statement about bytes Heyarr HOLDS — note this
    -- is checked against `managed` and NOT against the phase, because during
    -- an upgrade search a want is legitimately searching AND satisfied.
    CHECK (content <> 'satisfied' OR managed = 1),

    -- §56's forbidden combination: bytes cannot be placed on peers before
    -- there are bytes worth placing.
    CHECK (placement NOT IN ('satisfied', 'converging') OR content = 'satisfied')
) STRICT;

-- The reconciliation sweep (§57) walks by phase to find work in flight, and the
-- operational view wants "what is stuck" — both are table scans without this.
CREATE INDEX acquisition_state_by_phase ON acquisition_state (phase, phase_entered_at);

-- "What is not satisfied" is the question the whole milestone exists to answer,
-- and it is asked on every reconciliation pass.
CREATE INDEX acquisition_state_unsatisfied
    ON acquisition_state (content) WHERE content <> 'satisfied';

-- +goose Down
DROP INDEX acquisition_state_unsatisfied;
DROP INDEX acquisition_state_by_phase;
DROP TABLE acquisition_state;
