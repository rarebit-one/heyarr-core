-- +goose Up
-- What integrity and garbage collection need to be honest (M1-16, ADR-0018).

-- When a blob stopped being referenced by any asset.
--
-- ADR-0018's grace window is the whole safety argument — "a mistaken delete is
-- reversible for as long as it lasts" — and a window needs a start. Without
-- this column the only available timestamp is first_seen_at, which measures the
-- wrong thing entirely: a blob ingested a year ago and dereferenced a minute
-- ago would be eligible immediately, so the grace window would protect exactly
-- the blobs that never needed protecting.
--
-- Nothing sets it at delete time, because in Milestone 1 nothing deletes: an
-- asset stops referencing a blob by being replaced in place or removed by hand.
-- So garbage collection maintains it itself, mark-and-sweep style — one pass
-- observes "no references" and records when, a later pass past the window
-- reclaims. That also means the clock starts when Heyarr first NOTICED, which
-- is the conservative direction to be wrong in.
--
-- It is cleared again the moment a blob regains a reference, so a blob that
-- comes back gets a fresh, full window rather than a partly spent one.
ALTER TABLE blobs ADD COLUMN unreferenced_since TEXT;

-- Partial: the interesting set is tiny next to the table, and a healthy
-- instance keeps it empty.
CREATE INDEX blobs_unreferenced ON blobs (unreferenced_since)
    WHERE unreferenced_since IS NOT NULL;

-- The quarantine ledger.
--
-- ADR-0018 says a blob that fails verification is moved to quarantine/ and
-- never deleted, because it is evidence: on a hardlink-ingested library the
-- "corruption" may be the ORIGINAL file that legitimately changed under an
-- external tool, and on the reference host hardlink is the default ingest outcome for
-- every file (#43). Preserving the bytes without recording what was expected,
-- what was found and when is only half of that: a quarantined blob nobody can
-- explain later is barely better than a deleted one.
--
-- blob_hash deliberately has NO foreign key to blobs. Quarantine outlives the
-- catalog row — the point is that the evidence survives whatever happens next.
CREATE TABLE quarantine (
    id             TEXT PRIMARY KEY,           -- UUIDv7 (ADR-0017)
    blob_hash      TEXT NOT NULL
                   CHECK (blob_hash GLOB 'blake3:[0-9a-f]*' AND length(blob_hash) = 71),
    peer_id        TEXT NOT NULL REFERENCES peers (id) ON DELETE CASCADE,
    -- Where the bytes are now, so an operator can go and look at them.
    path           TEXT NOT NULL,
    reason         TEXT NOT NULL DEFAULT 'hash_mismatch',
    -- What the bytes actually hashed to. The expected digest is blob_hash: the
    -- name IS the expectation (ADR-0005).
    actual_hash    TEXT,
    size           INTEGER NOT NULL DEFAULT 0 CHECK (size >= 0),
    detail         TEXT NOT NULL DEFAULT '',
    quarantined_at TEXT NOT NULL
) STRICT;

CREATE INDEX quarantine_by_blob ON quarantine (blob_hash);

-- +goose Down
DROP INDEX quarantine_by_blob;
DROP TABLE quarantine;
DROP INDEX blobs_unreferenced;
ALTER TABLE blobs DROP COLUMN unreferenced_since;
