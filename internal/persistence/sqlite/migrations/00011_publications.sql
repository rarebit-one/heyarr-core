-- +goose Up
-- What a publication container declares about itself (§69, M2-08).
--
-- 00008 stays reserved for probe results (M2-04). M2-10's contingent
-- reservation moves to 00012.
--
-- # Keyed by blob hash, like a probe result
--
-- This describes BYTES, and bytes are identity (invariant 1, ADR-0005). Two
-- assets sharing an EPUB blob share its spine count, and re-ingesting the same
-- file re-uses the answer rather than re-reading the archive. It is also the
-- natural idempotency key: the examiner is safely re-runnable and converges on
-- one row.
--
-- The same consequence as probe results applies and is recorded in the same
-- place: a `linked` asset has no blob at all (ADR-0020), so a linked
-- publication cannot carry this metadata. Milestone 1 never wrote a linked
-- asset, so nothing is broken today — but it is the second place the linked
-- class's absence of a blob bites, and that is now a pattern rather than an
-- incident.
--
-- # Absent is not zero
--
-- page_count and chapter_count are NULLABLE and mean "not read", which is a
-- different answer from zero. An empty CBZ is a zero-page comic; a PDF is a
-- publication whose page count Heyarr does not read, because that needs a PDF
-- parser and §69 says Heyarr does not render. A client shown "0 pages" for the
-- second has been told something false.

CREATE TABLE publications (
    blob_hash TEXT PRIMARY KEY REFERENCES blobs (hash) ON DELETE CASCADE,

    format TEXT NOT NULL CHECK (format IN ('epub', 'pdf', 'cbz', 'cbr')),

    -- Page images in a comic archive.
    page_count INTEGER CHECK (page_count IS NULL OR page_count >= 0),
    -- Spine items in an EPUB — its reading order, which is the closest thing an
    -- EPUB has to a page count and is deliberately not called one.
    chapter_count INTEGER CHECK (chapter_count IS NULL OR chapter_count >= 0),

    -- When the container was last read. Without it, "we have never looked" and
    -- "we looked and it declared nothing" are the same row.
    examined_at TEXT NOT NULL
) STRICT;

CREATE INDEX publications_by_format ON publications (format);

-- +goose Down
DROP INDEX publications_by_format;
DROP TABLE publications;
