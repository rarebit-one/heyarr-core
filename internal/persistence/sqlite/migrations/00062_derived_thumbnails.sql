-- +goose Up
-- Derived thumbnail index (ADR-0109).
--
-- Thumbnails are normal CAS blobs — they are content-addressed JPEG bytes stored
-- in the same store as every other blob. This table is the index that maps a
-- source blob hash + target bucket (long-edge pixel limit) to the derived blob.
--
-- source_hash is not a foreign key on blobs, deliberately: the blob row may be
-- collected while this record still holds the answer about those bytes, and a
-- re-ingest of the same source should not re-derive. The GC sweep does its own
-- cascade: when a source blob is reclaimed, Reclaim() deletes the derivation rows
-- and emits TypeThumbnailDeleted per row in the same transaction (ADR-0018).
--
-- thumb_hash IS counted as a reference in the GC reference count (Blobs/Blob in
-- integrity.go), so a ready thumbnail holds its derived blob alive until the
-- derivation row is removed.
--
-- recipe covers the decoder+orientation+scaler+JPEG quality+format constants.
-- Rows of older recipes are deleted lazily when a current-recipe row becomes
-- ready for the same (source_hash, bucket) pair, so stale thumbs become
-- collectable via normal GC.
CREATE TABLE derived_thumbnails (
    source_hash TEXT    NOT NULL,
    bucket      INTEGER NOT NULL,
    recipe      INTEGER NOT NULL,
    status      TEXT    NOT NULL CHECK (status IN ('ready', 'undecodable')),
    -- thumb_hash is NULL when status = 'undecodable'.
    thumb_hash  TEXT,
    created_at  TEXT    NOT NULL,
    PRIMARY KEY (source_hash, bucket, recipe)
) STRICT;

-- +goose Down
DROP TABLE derived_thumbnails;
