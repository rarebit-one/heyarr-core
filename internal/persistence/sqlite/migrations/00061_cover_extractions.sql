-- +goose Up
-- Which book files have had their embedded cover looked for (ADR-0105).
--
-- The cover beat asks "which held books have no cover and a file nobody has
-- looked inside yet?". The job queue cannot answer the second half: its dedupe
-- key only covers LIVE jobs, and finished jobs are pruned. Without a durable
-- record, a PDF with no renderable first page — or an EPUB that declares no
-- cover — would be re-opened every pass forever.
--
-- Keyed on the blob, not the asset or the Work: identical bytes have an
-- identical cover, so one look covers every Edition holding them. No foreign
-- key, deliberately — a blob row may be collected while this answer stays true
-- of the bytes, and a re-ingest of the same bytes should not look again.
CREATE TABLE cover_extractions (
    blob_hash TEXT PRIMARY KEY,
    -- 'extracted' (a cover was recorded) or 'none' (the file declares none, or
    -- could not be read or rendered). Either way the bytes need no second look.
    outcome TEXT NOT NULL CHECK (outcome IN ('extracted', 'none')),
    -- What the file was, for an operator reading the table: 'epub' | 'pdf'.
    format TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    attempted_at TEXT NOT NULL
) STRICT;

-- +goose Down
DROP TABLE cover_extractions;
