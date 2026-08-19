-- +goose Up
-- The append-only event log (spec §76, ADR-0009).
--
-- Events ship in Milestone 1 deliberately. §61 names "polling as the only
-- integration model" as an *arr failure, but the practical argument is
-- narrower: retrofitting events means auditing every mutation site, and that
-- audit gets more expensive with every milestone.

CREATE TABLE events (
    -- Monotonic, gapless-by-construction ordering. A client reconnects with
    -- ?after=<seq> and resumes exactly where it left off (§76).
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    id           TEXT NOT NULL UNIQUE,      -- UUIDv7, stable across replay
    type         TEXT NOT NULL,             -- e.g. blob.created, job.failed
    subject_type TEXT NOT NULL DEFAULT '',
    subject_id   TEXT NOT NULL DEFAULT '',
    payload      TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload)),
    created_at   TEXT NOT NULL
) STRICT;

CREATE INDEX events_by_type ON events (type, seq);
CREATE INDEX events_by_subject ON events (subject_type, subject_id, seq);

-- +goose Down
DROP TABLE events;
