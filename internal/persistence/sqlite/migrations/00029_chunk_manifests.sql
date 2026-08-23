-- +goose Up
-- Chunk manifests, the local chunk index, and the third state `blobs.chunked`
-- could not express (§15, §16, §17, ADR-0005, ADR-0034, M5-03).
--
-- # The boolean was never true
--
-- 00002_core.sql:107 carried `chunked INTEGER NOT NULL DEFAULT 0`. Nothing has
-- ever written it, so it has been 0 on every row in every deployment since
-- Milestone 1 — and it was surfaced as a fact all the way out to the client
-- API, the CLI and a peer's catalog snapshot. That is the same shape as M4's
-- `unproven`: a field telling the truth only because the interesting case did
-- not exist yet.
--
-- §16 makes chunking LAZY and says small blobs may never require a manifest at
-- all, so the question has three answers, not two:
--
--   present       a manifest exists for these bytes
--   not_required  a decision was recorded that these bytes will never need one
--   undecided     nobody has decided yet
--
-- A boolean expresses two of those, and the one it cannot express is the one
-- replication has to branch on: `false` conflates "we decided not to" with "we
-- have not looked", and a caller that cannot tell them apart will generate a
-- manifest to find out. That is the trap. Asking whether a blob has a manifest
-- must never generate one (ADR-0034).
--
-- # Where the third state lives, and why it is not a column
--
-- The state is DERIVED in one read, from two independent facts:
--
--   the presence of a chunk_manifests row  ->  present
--   chunking_exempt_reason IS NOT NULL     ->  not_required
--   neither                                ->  undecided
--
-- It is deliberately not a `chunking_state` enum column on `blobs`, because
-- ADR-0034 makes a manifest DISPOSABLE: "a manifest may be discarded at any
-- time with no loss of correctness", and deleting every manifest is a named,
-- supported recovery action. A stored enum saying 'present' would survive that
-- delete and start lying — a second copy of a truth that the manifest table
-- already holds, drifting the moment anyone exercises the recovery path. So
-- only the fact that is NOT derivable from the manifest table gets a column:
-- the recorded policy decision that these bytes will never need one.
--
-- `chunked` is dropped rather than kept as a vestigial 0. A column nothing
-- writes and nothing reads is not harmless; it is the next reader's evidence.
-- The API keeps a `chunked` field (it is `required` in the OpenAPI, so
-- removing it would be a breaking change) but computes it honestly from the
-- state, and gains `chunk_manifest` alongside it, which can say all three
-- things. That is the M4-11 treatment: restate it honestly, do not delete it.
ALTER TABLE blobs DROP COLUMN chunked;

-- The recorded policy decision, and only that. NULL is not "no": NULL is
-- "nobody has said". The distinction is the entire point of this migration.
ALTER TABLE blobs ADD COLUMN chunking_exempt_reason TEXT
    CHECK (chunking_exempt_reason IS NULL OR length(chunking_exempt_reason) > 0);
ALTER TABLE blobs ADD COLUMN chunking_exempt_at TEXT;

-- # chunk_manifests — keyed by the blob's identity, and by nothing else
--
-- ADR-0034: "A manifest describes a blob. It is keyed by the blob's
-- whole-object digest and is never the key of anything." blob_hash is the
-- PRIMARY KEY, so a manifest is reachable only from the identity it describes;
-- there is no route from a chunk list back to a blob, by construction rather
-- than by convention.
--
-- The chunker parameters are stored, not assumed. Two manifests computed under
-- different chunk-size settings describe the same bytes and share no
-- boundaries, so a peer that upgrades its defaults must be able to tell that
-- the manifest it is holding is not comparable with the one it would compute
-- now. Without these three numbers that is unknowable and the failure is
-- silent: a diff against an incomparable manifest reports that nothing is
-- reusable, which looks exactly like a blob that genuinely changed.
--
-- digest is a digest OF THE MANIFEST, for the manifest's own integrity — a
-- destination handed one is entitled to check it arrived intact. ADR-0034 is
-- explicit that this "names the manifest. It is not an alias for the blob, it
-- does not appear in `blobs`, and nothing may resolve it to bytes."
CREATE TABLE chunk_manifests (
    blob_hash    TEXT PRIMARY KEY REFERENCES blobs (hash) ON DELETE CASCADE,
    -- The chunker that produced it. A string rather than a boolean because
    -- FastCDC is the first algorithm here, not the only conceivable one.
    algorithm    TEXT NOT NULL CHECK (length(algorithm) > 0),
    min_size     INTEGER NOT NULL CHECK (min_size > 0),
    avg_size     INTEGER NOT NULL CHECK (avg_size > 0),
    max_size     INTEGER NOT NULL CHECK (max_size > 0),
    chunk_count  INTEGER NOT NULL CHECK (chunk_count >= 0),
    -- What the chunks cover. Checked against blobs.size on read: a manifest
    -- that covers fewer bytes than the blob is a truncated manifest, and a
    -- reassembly from it is a valid-chunk, wrong-file blob.
    covered_size INTEGER NOT NULL CHECK (covered_size >= 0),
    digest       TEXT NOT NULL
                 CHECK (digest GLOB 'blake3:[0-9a-f]*' AND length(digest) = 71),
    generated_at TEXT NOT NULL
) STRICT;

-- # manifest_chunks — the order is the data
--
-- (blob_hash, idx) is the primary key, so the sequence is a stored fact rather
-- than an ordering a reader has to remember to ask for. Every read of this
-- table must ORDER BY idx. ADR-0034 spells out why in one sentence: "a set of
-- individually valid chunks assembled in the wrong order is a set of valid
-- chunks and the wrong file." A manifest read back as a SET passes every
-- per-chunk check and reassembles the wrong bytes.
--
-- `offset` and `length` are SQL keywords, hence byte_offset and byte_length —
-- quoting a column name in every query is a footgun waiting for the one query
-- that forgets.
--
-- ON DELETE CASCADE from chunk_manifests, deliberately: the rows are the
-- manifest. A "manifest" with no chunk rows is not a smaller manifest, it is a
-- manifest that claims a chunk_count it cannot produce, and the digest check
-- would then be the only thing standing between that and a reassembly.
CREATE TABLE manifest_chunks (
    blob_hash   TEXT NOT NULL REFERENCES chunk_manifests (blob_hash) ON DELETE CASCADE,
    idx         INTEGER NOT NULL CHECK (idx >= 0),
    byte_offset INTEGER NOT NULL CHECK (byte_offset >= 0),
    byte_length INTEGER NOT NULL CHECK (byte_length > 0),
    digest      TEXT NOT NULL
                CHECK (digest GLOB 'blake3:[0-9a-f]*' AND length(digest) = 71),
    PRIMARY KEY (blob_hash, idx)
) STRICT;

-- "Which blobs' manifests mention this chunk" — the M5-07 reuse question.
-- Note the shape: digest -> rows, plural. It is not UNIQUE and must never be,
-- because a chunk recurring in two blobs is the ordinary case chunk-level
-- deduplication exists for.
CREATE INDEX manifest_chunks_by_digest ON manifest_chunks (digest);

-- # local_chunks — what this node HOLDS, and where
--
-- The reuse question M5-07 asks is "do I already have these bytes somewhere",
-- and without an index it is a scan of every manifest. This is that index.
--
-- It is emphatically NOT an identity. A row says: this chunk digest occurs at
-- this offset of this blob, which this node holds. It answers "where can I get
-- these bytes from" and it must never be used to answer "which blob is this" —
-- the primary key is (digest, blob_hash, byte_offset) precisely so that one
-- digest maps to MANY rows and a lookup by digest cannot be mistaken for a
-- lookup of a blob (ADR-0034, "conflation by chunk list").
--
-- # The cascade is chosen, not inherited (M4-12)
--
-- FK to `blobs`, ON DELETE CASCADE: a claim to hold bytes at an offset of a
-- blob that no longer exists is a dangling claim, and a reuse index full of
-- dangling claims sends a transfer to fetch chunks from nowhere. When the blob
-- goes, the claims go with it.
--
-- FK to `chunk_manifests`: deliberately ABSENT. Dropping a manifest must not
-- drop the record of bytes this node is holding — ADR-0034's falsification
-- test is that deleting every manifest costs speed and nothing else, and an
-- index that evaporated with them would make it cost more than that.
CREATE TABLE local_chunks (
    digest      TEXT NOT NULL
                CHECK (digest GLOB 'blake3:[0-9a-f]*' AND length(digest) = 71),
    blob_hash   TEXT NOT NULL REFERENCES blobs (hash) ON DELETE CASCADE,
    byte_offset INTEGER NOT NULL CHECK (byte_offset >= 0),
    byte_length INTEGER NOT NULL CHECK (byte_length > 0),
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (digest, blob_hash, byte_offset)
) STRICT;

-- Re-indexing one blob replaces that blob's rows, so the delete is by blob.
CREATE INDEX local_chunks_by_blob ON local_chunks (blob_hash);

-- +goose Down
DROP INDEX local_chunks_by_blob;
DROP TABLE local_chunks;
DROP INDEX manifest_chunks_by_digest;
DROP TABLE manifest_chunks;
DROP TABLE chunk_manifests;
ALTER TABLE blobs DROP COLUMN chunking_exempt_at;
ALTER TABLE blobs DROP COLUMN chunking_exempt_reason;
-- Restored exactly as 00002_core.sql declared it, including the default that
-- made it 0 on every row.
ALTER TABLE blobs ADD COLUMN chunked INTEGER NOT NULL DEFAULT 0 CHECK (chunked IN (0, 1));
