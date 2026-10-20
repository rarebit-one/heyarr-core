-- +goose Up
-- When the SELECTED row was chosen (ADR-0119). searched_at is when the search
-- that listed a candidate ran; a person may override the scorer's pick hours
-- later, and the acquire op a sibling ranks by "newest selection wins" must
-- carry THAT moment, not the search's. NULL on rows that are not selected,
-- and on rows selected before this migration (readers fall back to
-- searched_at).
ALTER TABLE release_candidates ADD COLUMN selected_at TEXT;

-- +goose Down
ALTER TABLE release_candidates DROP COLUMN selected_at;
