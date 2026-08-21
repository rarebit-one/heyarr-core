-- +goose Up
-- Quality profiles: acceptable, preferred, fully satisfied (§62, M3-01).
--
-- The first migration of Milestone 3. The numbers for the milestone were
-- claimed up front — 00012 profiles, 00013 desired items, 00014 acquisition
-- state, 00015 providers, 00016 acquisitions, 00017 release candidates, with
-- 00018 held — because two M1 branches once took non-contiguous ones and broke
-- two rollback tests. Gaps are safe; collisions are not.
--
-- # Three sections, three semantics
--
-- §62 gives a profile three parts and they are three KINDS of statement, not
-- three degrees of one: `accept` is a gate, `prefer` is a score, `terminal` is
-- a stop condition. They are three columns rather than one rules table with a
-- section discriminator for the same reason the device capability lists are
-- JSON: nothing queries by rule. The evaluator (M3-04) is a pure function that
-- takes the whole profile as a value, so there is no index to want here, and a
-- rules table would buy a join and a section CHECK in exchange for nothing.
--
-- # An empty terminal is the same as an absent one
--
-- Both mean "there is no condition under which this profile is finished",
-- which is legal and means "never stop looking" — the `archival` default is
-- exactly that. So the column defaults to an empty array rather than being
-- nullable: a difference between NULL and [] here would be one no operator
-- could articulate, and every read path would have to handle both.
--
-- # Why the shape is validated in Go and only asserted here
--
-- json_type is checked because a scalar smuggled into a list column produces
-- an evaluator that silently matches nothing — the same failure the devices
-- table guards against. What is NOT checked here is the vocabulary: whether
-- `resolution` exists, whether `gte` applies to it, whether a weight belongs
-- on a gate. That lives in internal/domain/policy and runs at write time,
-- because a mistake in a profile should be reported to whoever wrote the
-- profile, and a CHECK constraint cannot say "use one of accept, prefer,
-- terminal".

CREATE TABLE quality_profiles (
    id   TEXT PRIMARY KEY,          -- UUIDv7 (ADR-0017)

    -- The name is the identity an operator uses and the key seeding converges
    -- on: §55's DesiredItem names a profile, and it names it by something a
    -- person typed. UNIQUE so that a restart cannot produce a second
    -- `living-room` and leave every DesiredItem pointing at whichever one it
    -- found first.
    name        TEXT NOT NULL UNIQUE CHECK (length(trim(name)) > 0),
    description TEXT NOT NULL DEFAULT '',

    accept   TEXT NOT NULL DEFAULT '[]' CHECK (json_type(accept)   = 'array'),
    prefer   TEXT NOT NULL DEFAULT '[]' CHECK (json_type(prefer)   = 'array'),
    terminal TEXT NOT NULL DEFAULT '[]' CHECK (json_type(terminal) = 'array'),

    -- Whether this profile was seeded (M3-01's Defaults) or authored.
    --
    -- It exists so that seeding can converge without overwriting: an operator
    -- who edits `living-room` keeps their edit instead of having it replaced
    -- every morning by whatever the Go file happens to say. Without a way to
    -- tell the two apart, the only safe seeding policy is "insert once and
    -- never touch again", which means a corrected default never reaches an
    -- installation that already started.
    seeded INTEGER NOT NULL DEFAULT 0 CHECK (seeded IN (0, 1)),

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- Profiles are listed by keyset over name, which is unique, matching every
-- other collection in the API.
CREATE INDEX quality_profiles_by_name ON quality_profiles (name);

-- +goose Down
DROP INDEX quality_profiles_by_name;
DROP TABLE quality_profiles;
