-- +goose Up
-- Followed sources and the byte-less Item (§55, M12, ADR-0056, ADR-0057).
--
-- 00039 is the encrypted-snapshots table (M9). This is the first M12 migration:
-- the two new entities a followed source needs, landed before 00041 widens
-- desired_items to point at an Item, because that widening's item_id FK
-- references the `items` table created here.
--
-- # The Item is a byte-less row between Edition and Asset (ADR-0056)
--
-- §11's content spine is Work — Edition — Asset — Blob, and an episode nobody
-- has yet fits none of them: an Asset is a file that EXISTS, and there was no
-- entity for "the fifth episode of season two, which I do not have". The Item is
-- that entity. It carries WHAT a source emitted — a source-stable key, a title,
-- an air/publish date, per-type attributes — and no bytes: acquiring it produces
-- an Asset and a Blob the ordinary way, and the Item is what the per-episode
-- want points at (00041).
--
-- Attributes are JSON rather than columns for the reason works.attributes are:
-- a season/episode number is a TV fact, an enclosure hint is a podcast fact, and
-- a fourth source type must be an addition to a map rather than a migration that
-- adds two nullable columns nothing else reads (ADR-0056).

CREATE TABLE items (
    id TEXT PRIMARY KEY,                    -- UUIDv7 (ADR-0017)

    -- The Work every Item belongs to — the series, the podcast, the channel.
    -- Always set: an Item is a part OF a work (§11), and it is what a projected
    -- item-scoped want anchors to. CASCADE because an Item of a work that no
    -- longer exists is a dangling reference, not history worth keeping.
    work_id TEXT NOT NULL REFERENCES works (id) ON DELETE CASCADE,

    -- The Edition grouping this Item belongs to — a season, for a series.
    -- Nullable: a podcast entry or a video need not belong to an edition, and
    -- the grouping is discovered as the feed is walked rather than required up
    -- front. CASCADE for the same reason as work_id.
    edition_id TEXT REFERENCES editions (id) ON DELETE CASCADE,

    -- The source-stable identity the feed adapter supplies — an "S02E05", a
    -- podcast GUID, a video id. It is what dedupes an Item across polls: the
    -- poll loop diffs a feed's items by this key, and a key it has seen is not a
    -- new Item. Unique within a work, below.
    item_key TEXT NOT NULL,

    title TEXT NOT NULL DEFAULT '',

    -- When the source emitted it — an air date, a publish date. Nullable rather
    -- than a zero timestamp, because "the source did not say" is a real and
    -- distinct answer from any particular instant.
    published_at TEXT,

    -- Per-item, per-type facts (a season/episode number, a duration). A map so a
    -- new source type is not a new column (ADR-0056).
    attributes TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(attributes)),

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    -- One Item per (work, source-stable key). This is what makes the upsert on
    -- every poll idempotent (invariant 9): re-enumerating a feed re-presents the
    -- same keys, and a key already stored is not a new row.
    UNIQUE (work_id, item_key)
) STRICT;

-- The poll diff reads a work's items to find which keys are new, and read paths
-- list a work's items in key order.
CREATE INDEX items_by_work ON items (work_id, item_key);

-- Followed sources: a standing subscription that archives everything a source
-- emits, under one policy (ADR-0057). Control-plane state, single-writer,
-- modelled on desired_items.
CREATE TABLE follow_sources (
    id TEXT PRIMARY KEY,                    -- UUIDv7 (ADR-0017)

    -- The Work every projected Item belongs to — the series. CASCADE: a
    -- subscription to a work that no longer exists is not history.
    work_id TEXT NOT NULL REFERENCES works (id) ON DELETE CASCADE,

    -- The inferred source type. Stored so the poll loop knows which feed adapter
    -- to ask, never a caller's knob (ADR-0057). Phase 1 stores only 'tv_series';
    -- the CHECK names all four so a later phase is an addition, and refuses a
    -- typo rather than storing a source that is never polled.
    type TEXT NOT NULL CHECK (type IN ('tv_series', 'podcast', 'youtube_channel', 'rss_feed')),

    -- The source-stable handle the feed adapter resolves to enumerate items — a
    -- TVDB series id, a feed URL. OPAQUE here: the domain does not parse it, the
    -- adapter does, which is what keeps the control plane source-agnostic.
    feed_ref TEXT NOT NULL,

    -- The standard every projected item is measured against (§62). RESTRICT, as
    -- on desired_items: deleting the profile a subscription archives at while
    -- leaving the subscription would make every projected want unevaluable.
    quality_profile_id TEXT NOT NULL REFERENCES quality_profiles (id) ON DELETE RESTRICT,

    -- Carried onto every projected want: keep looking for a better copy (§60).
    monitor INTEGER NOT NULL DEFAULT 1 CHECK (monitor IN (0, 1)),

    -- How much back-catalogue to pull on the first poll.
    backfill TEXT NOT NULL DEFAULT 'from_now' CHECK (backfill IN ('from_now', 'full')),

    -- Free text an operator may attach. Never interpreted; carried onto projected
    -- wants so a want six months later can say where it came from.
    reason TEXT NOT NULL DEFAULT '',

    -- Poll bookkeeping. The follow beat is the search beat's sibling (ADR-0057):
    -- it ticks often and polls a source on next_poll_at, backing a quiet source
    -- off toward a ceiling. Kept ON THE SOURCE row rather than in a sibling table
    -- because it is strictly 1:1 with the source, and a NULL next_poll_at means
    -- "never polled" — the most-due state there is, exactly as a missing
    -- search_schedule row is for a want.
    poll_schedule    TEXT NOT NULL DEFAULT 'feed-poll',
    poll_fruitless   INTEGER NOT NULL DEFAULT 0,
    last_polled_at   TEXT,
    next_poll_at     TEXT,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,

    -- One subscription per (work, feed). Following the same series through the
    -- same feed twice is one subscription written twice, not two.
    UNIQUE (work_id, feed_ref)
) STRICT;

-- The follow beat lists sources due a poll, ordered by how overdue they are, so
-- a batch limit truncates the least urgent.
CREATE INDEX follow_sources_by_next_poll ON follow_sources (next_poll_at);
CREATE INDEX follow_sources_by_work ON follow_sources (work_id);

-- +goose Down
DROP INDEX follow_sources_by_work;
DROP INDEX follow_sources_by_next_poll;
DROP TABLE follow_sources;
DROP INDEX items_by_work;
DROP TABLE items;
