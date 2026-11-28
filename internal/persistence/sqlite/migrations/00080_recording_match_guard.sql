-- +goose Up
-- Recording-specific works require readers that enforce metadata-match evidence.
-- Advancing the schema version prevents older binaries from opening a restored
-- database containing those works without the playback and satisfaction gates.
SELECT 1;

-- +goose Down
-- Refuse a downgrade while an older binary could misinterpret a recording.
CREATE TEMP TABLE recording_downgrade_guard (n INTEGER NOT NULL CHECK (n = 0));
INSERT INTO recording_downgrade_guard SELECT COUNT(*) FROM works WHERE work_key LIKE 'music-recording:%';
DROP TABLE recording_downgrade_guard;
