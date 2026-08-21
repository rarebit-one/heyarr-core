-- +goose Up
-- Release candidates and their evaluations (§63, M3-12).
--
-- 00012 quality profiles, 00013 desired items, 00014 acquisition state,
-- 00015 providers, 00016 acquisitions. 00018 remains held.
--
-- # Why candidates are stored at all
--
-- §63 says evaluation is deterministic and INSPECTABLE. An evaluation that
-- lived in memory for two hundred milliseconds is deterministic and is not
-- inspectable: "why did it pick that release, last Tuesday, at 3am" cannot be
-- answered by re-running a scorer over candidates nobody kept.
--
-- The rejections matter more than the acceptance. A search that found twelve
-- releases and took none of them must leave twelve explained refusals, or the
-- want simply goes quiet — and §60 keeps explainable rejection reasons among
-- the things Heyarr retains precisely because that silence is the failure mode
-- of the software it replaces.
--
-- # The evaluation is stored VERBATIM
--
-- `evaluation` holds the JSON encoding of what M3-04 returned, byte for byte.
-- Not a summary, not a re-derivation, and deliberately not decomposed into
-- columns.
--
-- Decomposing it would mean this table's shape had to track the evaluator's,
-- and the two would drift — at which point the stored explanation stops being
-- the explanation that was actually used, which is the only property that
-- makes it worth storing. accepted/score/terminal ARE duplicated out as
-- columns, but only so the table can be queried and ordered; they are a
-- projection of the blob, and the blob is the record.
--
-- # One search's answers replace the previous one
--
-- A search supersedes its predecessor: the previous answer was computed
-- against an indexer's state that no longer exists, and keeping both would
-- make "what are the candidates for this want" a question with an ORDER BY and
-- a LIMIT — the sort of thing that is right until somebody forgets the ORDER
-- BY. Replacement also makes the table self-limiting in the normal path,
-- bounded by (wants x candidates per search).
--
-- What replacement does NOT bound is a want nobody searches again: its last
-- set would sit here forever. That is what PruneCandidates is for, and it runs
-- globally on every search rather than per want, because a per-want prune can
-- never reach the wants that stopped being searched.

CREATE TABLE release_candidates (
    id TEXT PRIMARY KEY,                    -- UUIDv7 (ADR-0017)

    -- CASCADE: candidates explaining a want that no longer exists are not
    -- history worth keeping, they are a dangling reference every read path
    -- would have to special-case.
    desired_item_id TEXT NOT NULL
        REFERENCES desired_items (id) ON DELETE CASCADE,

    -- Which search produced this row. One search's answers share an id, so a
    -- set can be replaced, listed and reasoned about as the unit it is.
    search_id TEXT NOT NULL,

    -- The provider's own name for itself (§59), and the provider's own
    -- identity for the release.
    --
    -- Not a foreign key to anything: provider configuration lives in the
    -- config file, and a provider commented out for an afternoon must not
    -- cascade away the explanation of what it once offered.
    provider     TEXT NOT NULL,
    candidate_id TEXT NOT NULL CHECK (length(candidate_id) > 0),

    title      TEXT NOT NULL DEFAULT '',
    attributes TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(attributes)),

    -- §63's answer, verbatim. See above.
    evaluation TEXT NOT NULL CHECK (json_valid(evaluation)),

    -- A projection of `evaluation`, so the table can be ordered and filtered
    -- without decoding every row. Never the source of truth.
    accepted INTEGER NOT NULL CHECK (accepted IN (0, 1)),
    score    INTEGER NOT NULL,
    terminal INTEGER NOT NULL CHECK (terminal IN (0, 1)),

    -- The one this want is acquiring, when one was chosen.
    selected INTEGER NOT NULL DEFAULT 0 CHECK (selected IN (0, 1)),

    -- Whether a PERSON chose it against the scorer's ranking (§60's manual
    -- override), and what the scorer had said instead.
    --
    -- An override that left no trace would turn a deterministic scorer into
    -- something whose history cannot be reconstructed: the row would look
    -- exactly like an ordinary selection, and "why did it take that one" would
    -- have no answer at all.
    overridden       INTEGER NOT NULL DEFAULT 0 CHECK (overridden IN (0, 1)),
    override_detail  TEXT NOT NULL DEFAULT '',

    -- When the search that produced this ran. Distinct from created_at only in
    -- principle, and kept separate because the prune reasons about the SEARCH
    -- rather than about the row.
    searched_at TEXT NOT NULL,
    created_at  TEXT NOT NULL,

    -- A candidate is identified by (provider, its own id), and the same
    -- release offered twice for one want is one candidate. The registry
    -- already deduplicates across indexers; this is the floor under it.
    UNIQUE (desired_item_id, provider, candidate_id),

    -- An override is a KIND of selection. A row marked overridden but not
    -- selected would be a record of a choice nobody made.
    CHECK (overridden = 0 OR selected = 1),
    -- And an override has to say what it overrode, or it is exactly the
    -- traceless override this column exists to prevent.
    CHECK (overridden = 0 OR length(override_detail) > 0)
) STRICT;

-- At most one selected candidate per want.
--
-- The same idiom as peers_only_one_self: two rows claiming to be the chosen
-- release would make "what are we acquiring" ambiguous, and the ambiguity
-- would present as an intermittently wrong download rather than as an error.
CREATE UNIQUE INDEX release_candidates_one_selected
    ON release_candidates (desired_item_id) WHERE selected = 1;

-- Listing a want's candidates, best first, which is how the API returns them
-- and how a person reads them.
CREATE INDEX release_candidates_by_want
    ON release_candidates (desired_item_id, accepted DESC, score DESC, candidate_id);

-- The prune walks by age.
CREATE INDEX release_candidates_by_age ON release_candidates (searched_at);

-- +goose Down
DROP INDEX release_candidates_by_age;
DROP INDEX release_candidates_by_want;
DROP INDEX release_candidates_one_selected;
DROP TABLE release_candidates;
