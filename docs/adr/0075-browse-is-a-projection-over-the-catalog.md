# 0075. Browse is a projection over the catalog

**Status:** Accepted
**Date:** 2026-09-05
**Builds on:** ADR-0013 (one byte endpoint), ADR-0017 (time, identifiers, determinism), ADR-0020 (managed / linked / vault assets), ADR-0024 (one consumption-session model), ADR-0054 (the first-party client is the product; compat adapters are reach), ADR-0056 (the Item entity), ADR-0074 (guest is a first-class read-only mode)
**Tracks:** #456

## Context

The first-party client (ADR-0054) wants to open on a shelf, not a ledger: a
poster per work, a "recently added" row, a "continue" row, hubs per media kind,
one search box, and a tap that plays. The API served none of that. A work's
artwork existed only as an `assets.role = 'artwork'` row with no endpoint;
`GET /works` paged by title with no other order and no facets; a listing row
named no playable file, so a tap needed a second read; `POST /search` matched
titles over works only; and the only artist and author groupings lived inside
the Subsonic and OPDS adapters, each with its own SQL.

Two shapes were open. **Grow the model** — an `artworks` table, an `artists`
entity, a `collections` table, a thumbnail cache — or **project what the
catalog already holds** — the artwork asset a scan already recorded, the
`created_at` a row already carries, the `attributes.artist` the identifier
already wrote — and add nothing a rescan would not re-derive.

The second is the one consistent with ADR-0073's finding that the catalog's
*base* converges because it is re-derivable from bytes. A browse layer that
introduced rows a scan does not produce would be a second editorial layer to
converge across sites. A projection has nothing to converge.

## Decision

**Browse reads are projections over the existing catalog. Nothing here adds a
table, an entity or a stored image.**

1. **Artwork is the ranked `artwork`-role asset, and the artwork route
   redirects to its blob.** `GET /works/{id}/artwork` answers `307` to
   `/api/v1/blobs/{hash}/content`. The blob route is the one contract for bytes
   (ADR-0013), and its immutable, year-long cache headers are correct only
   because the URL is the hash; a work's poster can change, so a work-keyed
   image URL must not carry them. The picker prefers a `poster`/`cover`/
   `folder`/`front` over an unnamed image over `fanart`/`backdrop`/`banner`,
   breaks ties by id (ADR-0017), and considers only an asset that holds bytes
   and is not missing — a `linked` artwork has no blob (ADR-0020) and nothing
   serves it. **There is no `?size=`.** The node has no image library and no
   thumbnail cache; a client sizes what it fetches.

2. **Per-row embeds are opt-in on listings and always present on the detail
   read.** `GET /works?include=artwork,primary_asset` adds `artwork` and
   `primary_asset` to each row; without `include` the rows are plain works,
   byte-identical to before. An embed not asked for is *absent*; one asked for
   that the work lacks is `null` — "I did not ask" and "there is no poster" are
   different answers. `GET /works/{id}` carries both always: one row, two cheap
   lookups. `primary_asset` is the first `primary`-role asset holding bytes,
   with its size and probed duration inlined, so a tap can go straight to
   `POST /playback/plan`.

3. **"Recently added" is an order, under its own cursor.** `sort=recent` keys
   on `(created_at DESC, id DESC)` and its cursor names the collection
   `works-recent`, so a title-order cursor is refused on it rather than read
   as a position in a different order. Filters `year`, `year_from`, `year_to`,
   `artist` and `author` narrow either order.

4. **An artist, an author, is a grouping, not an entity.** The music and book
   identifiers write `attributes.artist` and `attributes.author`; `GET /artists`
   and `GET /authors` group works on those, keyed by the name, with a
   representative artwork and a count — the stance the Subsonic adapter already
   took. An artist's albums are `GET /works?content_type=music&artist=<name>`.

5. **Continue is a fold over consumption sessions, per work.** The newest
   session per work that holds a position and is not completed
   (`playing | paused | stopped`, a non-empty locator), bounded by `limit`,
   with the work, edition, asset and duration inlined. It is per-identity
   history (ADR-0024) and therefore refused to a Guest (ADR-0074).

6. **Search stays one intent, two doors.** `POST /search` returns works with
   their artwork and attributes, and episode hits from both series editions
   and followed-source items (ADR-0056); the MCP `search_content` tool calls
   the same function rather than carrying its own query.

Items 4–6 are specified here and land in follow-up increments under #456;
items 1–3 land with this record.

## Consequences

- A client can draw a shelf from one listing read and play from a card tap
  without a second read. A Guest sees the same shelf minus anything vault.
- Every browse read applies the guest boundary through the same picker, so a
  vault poster is invisible to a Guest on the redirect, the embed and the
  grouping alike.
- The Subsonic `getCoverArt` gap and the OPDS thumbnail link can be filled by
  the same picker rather than by a third query.
- Nothing new converges across sites (ADR-0073): every projection re-derives
  from rows a rescan produces.

## Revisit when

- **Poster bytes cost too much on a real library.** Measure first. If a phone
  fetching a 4 MB scan for a 200 px card is what a real shelf looks like, an
  image library and a content-addressed thumbnail cache earn `?size=`; the
  redirect shape survives that (it would redirect to the thumbnail's blob).
- **A metadata provider supplies artist identifiers** (MusicBrainz ids). A
  name-keyed grouping cannot merge "Artist" and "Artist (feat. …)" or split two
  artists sharing a name; an id-keyed one can, and that is the day an artist
  entity is justified.
- **Principals bind devices.** Continue is per-device today because sessions
  are; once a principal spans devices, the fold is per person.
- HLS/ABR stays #202; playlists and history stay device-side (ADR-0054, M9).
