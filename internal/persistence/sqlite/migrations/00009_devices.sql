-- +goose Up
-- Device capability profiles: the second of the three inputs the playback
-- planner needs (§68, M2-05).
--
-- 00008 is reserved for the probe results (M2-04), which is the first input.
-- The numbers for the whole milestone were claimed up front because two M1
-- branches once took non-contiguous ones and broke two rollback tests.
--
-- # A device here is a capability profile, not an identity
--
-- ADR-0022 covers device enrolment, keypairs and key recovery, and belongs to
-- Milestone 8. Nothing in this table is a credential and nothing here
-- authenticates: a client says what it can play, and Heyarr believes it,
-- because Heyarr has no way to interrogate a television. §84's ordering is
-- load-bearing, and a device row that grew a public key here would have to be
-- retrofitted around a personal-state plane it was designed without.
--
-- The shape is chosen so Milestone 8 can ADD identity to a device rather than
-- replace it. There is deliberately no principal_id either: whose device this
-- is only becomes answerable once there are principals worth distinguishing,
-- and a nullable column nobody writes is how the linked and vault asset
-- classes ended up existing in the schema and never being written.

CREATE TABLE devices (
    id          TEXT PRIMARY KEY,           -- UUIDv7 (ADR-0017)

    -- The client's own stable identifier, and the idempotency key for
    -- registration. An app announces itself on every launch; without this,
    -- the table accumulates one row per launch and ends up with four thousand
    -- devices called "Living Room".
    device_key  TEXT NOT NULL UNIQUE,

    name        TEXT NOT NULL,
    platform    TEXT NOT NULL DEFAULT '',   -- free text: tvos, android, web

    -- Scalars are columns because their invariants are expressible here, and
    -- the invariant is the database's job rather than the caller's — the same
    -- reasoning as the assets source_class CHECK.
    --
    -- Zero means "no limit stated", which is different from a small limit and
    -- must stay tellable apart: a device that declares no maximum bitrate is
    -- not a device that declares a maximum of zero. The API rejects a negative
    -- or absurd value before it reaches here; these CHECKs are the floor.
    max_width      INTEGER NOT NULL DEFAULT 0 CHECK (max_width  >= 0),
    max_height     INTEGER NOT NULL DEFAULT 0 CHECK (max_height >= 0),
    max_bitrate_bps INTEGER NOT NULL DEFAULT 0 CHECK (max_bitrate_bps >= 0),
    supports_hdr   INTEGER NOT NULL DEFAULT 0 CHECK (supports_hdr IN (0, 1)),

    -- The lists are JSON because their length varies and nothing queries by
    -- codec: the planner (M2-07) is a pure function that takes the whole
    -- profile as a value, so there is no index to want here. Same reasoning as
    -- works.attributes — §12 lists thirteen content types and a device profile
    -- will grow per-type fields.
    --
    -- json_type is asserted rather than only json_valid: `"h264"` is valid
    -- JSON and is not a list of codecs, and a scalar smuggled into a list
    -- column produces a planner that silently matches nothing.
    containers   TEXT NOT NULL DEFAULT '[]' CHECK (json_type(containers)   = 'array'),
    video_codecs TEXT NOT NULL DEFAULT '[]' CHECK (json_type(video_codecs) = 'array'),
    audio_codecs TEXT NOT NULL DEFAULT '[]' CHECK (json_type(audio_codecs) = 'array'),

    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    -- When this device last registered. It is the only thing that makes a
    -- stale row identifiable later; without it, a device retired two years ago
    -- and one used this morning are indistinguishable.
    last_seen_at TEXT NOT NULL
) STRICT;

-- Devices are listed by keyset over (name, id), matching every other
-- collection in the API. The id is in the sort key because name is not unique
-- and a non-unique keyset boundary skips and repeats rows exactly as OFFSET
-- does.
CREATE INDEX devices_by_name ON devices (name, id);

-- +goose Down
DROP INDEX devices_by_name;
DROP TABLE devices;
