-- +goose Up
-- Tag each quality profile with which content type(s) it is meant for
-- (operator request, 2026-09-20): a client offering a profile picker for a
-- book want should not offer "living-room" — a video-only profile whose
-- accept gate (a resolution floor) a book can never pass, so it would only
-- ever refuse. Nothing on a profile said what it was FOR before this column,
-- only what its rules happened to check.
--
-- Empty content_types means "applies to any content type" — the safe default
-- for every profile that predates this column, so nothing that already
-- worked stops working. It also stays the honest answer for a profile that
-- genuinely is not content-type-specific: `subtitle` is aspect-scoped, not
-- content-type-scoped (ADR-0085), and `indexer-determinable` is a diagnostic
-- profile (#129), not a real-world choice a person picks for a want.
ALTER TABLE quality_profiles ADD COLUMN content_types TEXT NOT NULL DEFAULT '[]'
    CHECK (json_type(content_types) = 'array');

-- Backfill the profile names this schema version knows about. Each is a
-- no-op UPDATE where the name is absent, so this is safe against a fresh
-- install with none of them yet, or one with only some (an operator's own
-- profiles, named anything else, are simply left at the unrestricted default
-- — this cannot guess what an operator meant a profile they authored for).
UPDATE quality_profiles SET content_types = '["movie","series"]'
    WHERE name IN ('living-room', 'everyday', 'archival', 'everyday-en', 'livingroom-en');
UPDATE quality_profiles SET content_types = '["podcast","document"]'
    WHERE name = 'published';
UPDATE quality_profiles SET content_types = '["book"]'
    WHERE name = 'ebook';

-- +goose Down
ALTER TABLE quality_profiles DROP COLUMN content_types;
