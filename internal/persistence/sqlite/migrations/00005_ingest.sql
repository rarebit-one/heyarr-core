-- +goose Up
-- What ingest needs to be idempotent (M1-10, ADR-0008).
--
-- The job queue WILL re-run ingest_artifact: a lease can expire under a slow
-- hash of a 60 GB remux and the reaper hands the job back. Re-running must
-- converge on the same rows rather than multiply them, and "converge" needs a
-- key the database can enforce. Without these, idempotency would be a property
-- of the handler's control flow — which is to say, a property nobody enforces.

-- An edition's identity within its work. M1-11 derives it from the path
-- ("2160p remux", "Season 02", "FLAC"); later milestones may derive it from a
-- metadata provider. Either way, get-or-create needs something unique.
ALTER TABLE editions ADD COLUMN edition_key TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX editions_by_key ON editions (work_id, edition_key);

-- Which rule produced the identification, alongside identification_source.
-- Milestone 3's real identifier re-resolves these rows, and "which heuristic
-- decided this" is the difference between re-resolving deliberately and
-- guessing (M1-11).
ALTER TABLE assets ADD COLUMN identification_rule TEXT NOT NULL DEFAULT '';

-- Per-file identification facts. The identifier produces two attribute sets:
-- one about the Work (an album's artist, a book's author) and one about this
-- particular file's placement (season, episode, disc, track). The second is NOT
-- an Edition fact, even though the Edition is what it helps name: every episode
-- file in season 2 resolves to the same Edition row, so writing episode numbers
-- there means the row describes whichever file happened to be scanned first.
-- Placement belongs to the asset, which is the thing there is one of per file.
ALTER TABLE assets ADD COLUMN attributes TEXT NOT NULL DEFAULT '{}';

-- A managed asset's identity for ingest purposes is where its bytes were found.
-- Not the blob hash: two paths holding identical bytes are two assets sharing
-- one blob (§13), and keying on the hash would collapse them into one.
CREATE UNIQUE INDEX assets_managed_source_path ON assets (library_id, source_path)
    WHERE source_class = 'managed' AND source_path IS NOT NULL;

-- +goose Down
DROP INDEX assets_managed_source_path;
ALTER TABLE assets DROP COLUMN attributes;
DROP INDEX editions_by_key;
ALTER TABLE assets DROP COLUMN identification_rule;
ALTER TABLE editions DROP COLUMN edition_key;
