-- +goose Up
-- Let a followed source want subtitles per language (ADR-0085 §6).
--
-- A subscription can now stand for more than "archive each episode": it can also
-- want a subtitle, in one or more languages, for every episode it projects — so
-- the fetch driver acquires each caption once the episode's video lands, with no
-- per-title request. The languages are a JSON array of ISO-639-1 codes; "[]" is
-- the default and means the subscription wants no subtitle beyond whatever a
-- release ships or a container embeds.
--
-- A plain ADD COLUMN — no CHECK change — so no table rebuild. NOT NULL DEFAULT
-- '[]' so every existing subscription reads as "no subtitle languages wanted",
-- which is exactly their behaviour before this column existed.
ALTER TABLE follow_sources ADD COLUMN want_subtitles TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE follow_sources DROP COLUMN want_subtitles;
