-- +goose Up
-- What ffprobe said about a blob's bytes (§29, M2-04).
--
-- This number was reserved when the milestone started, before 00009 (devices),
-- 00010 (consumption sessions) and 00011 (publications) were written. The gap
-- is why the rollback test loops on the version rather than counting steps —
-- two M1 branches once took non-contiguous numbers and broke it.
--
-- # Keyed by blob hash
--
-- A probe describes BYTES, and bytes are identity (invariant 1, ADR-0005). Two
-- assets sharing a blob share its probe; re-ingesting the same file re-uses the
-- answer rather than re-probing 20 GB. It is also the natural idempotency key,
-- which matters because a probe job WILL be re-run (invariant 9) and must
-- converge on one row rather than accumulate.
--
-- The same consequence as publications (00011), and now a pattern rather than
-- an incident: a `linked` asset has no blob at all (ADR-0020), so a linked
-- asset cannot be probed. ADR-0020 nonetheless calls linked assets playable.
-- Milestone 1 never wrote a linked asset so nothing is broken today, but this
-- is the THIRD place the linked class's absence of a blob bites, and it should
-- be answered deliberately rather than accreted around. It said "in Milestone
-- 5"; Milestone 5 has been and gone without answering it, because it was the
-- milestone that made replication cheaper. The milestone is dropped rather than
-- advanced to the next one — a date that keeps moving is a promise nobody made.

CREATE TABLE blob_probes (
    blob_hash TEXT PRIMARY KEY REFERENCES blobs (hash) ON DELETE CASCADE,

    -- ffprobe's format_name verbatim: a comma-separated list of every name the
    -- demuxer answers to, e.g. "mov,mp4,m4a,3gp,3g2,mj2". Kept whole rather
    -- than reduced to one name, because which of those a file "is" is a
    -- question with no answer, and the planner matches by membership.
    container   TEXT NOT NULL,
    format_long TEXT NOT NULL DEFAULT '',

    duration_seconds REAL CHECK (duration_seconds IS NULL OR duration_seconds >= 0),
    bitrate_bps      INTEGER CHECK (bitrate_bps IS NULL OR bitrate_bps >= 0),

    -- The streams, as the probe reported them. JSON for the same reason
    -- works.attributes is: the shape varies per codec — a video stream has a
    -- resolution and an audio stream has a channel count — and nothing queries
    -- by an individual stream field. The planner (M2-07) is a pure function
    -- over the whole result.
    streams TEXT NOT NULL DEFAULT '[]' CHECK (json_type(streams) = 'array'),

    -- How the probe was obtained, so the §29 claim is auditable in production
    -- rather than only in a test. A deployment where every probe materialised
    -- is a deployment where remote probing is silently not working, and
    -- without these columns the only symptom is that it feels slow.
    bytes_read   INTEGER NOT NULL DEFAULT 0 CHECK (bytes_read >= 0),
    materialised INTEGER NOT NULL DEFAULT 0 CHECK (materialised IN (0, 1)),

    probed_at TEXT NOT NULL
) STRICT;

-- The interesting query is "what fell back", which is a small set on a healthy
-- instance and the first thing to look at on an unhealthy one.
CREATE INDEX blob_probes_materialised ON blob_probes (materialised) WHERE materialised = 1;

-- +goose Down
DROP INDEX blob_probes_materialised;
DROP TABLE blob_probes;
