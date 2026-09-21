# 0099. Discovery gains a want-scoped candidate; TMDB movies, Open Library books and MusicBrainz music become discoverable

**Status:** Accepted
**Date:** 2026-09-20
**Milestone:** M12 — Followed Sources / The Archive

## Context

ADR-0077 added TMDB as a second `CapabilityMetadata` provider behind
`DiscoverySearcher`, serving `tv_series` only, and deliberately deferred
movies, books and music. It typed `DiscoveryCandidate.Type` as `followed.Type`
— a type whose only values are the four feed-shaped followed source kinds
(`tv_series`, `podcast`, `youtube_channel`, `rss_feed`) — because nothing else
had anywhere to route: a movie, a book, an album is not a feed, has no
calendar, and `follow_source` has no door for one. That ADR named the fix
explicitly: "the missing piece is a non-feed, want-scoped discovery candidate
(a discovery result a caller `want`s rather than `follow`s)."

Operator request (2026-09-20): universal search should also surface results
from metadata providers for movies, books and music — not just TV series —
so unheld content can be found and acquired, not only what the library
already has.

This is exactly the boundary ADR-0077 marked as the next attempt's starting
point. It is NOT the other half that ADR-0077 flagged as separately
milestone-scale: a real acquisition pipeline that can search for and GRADE
music/book releases (indexers + a quality vocabulary that understands
bitrate, format, edition) remains future work. What this ADR builds is
narrower and already fully supported by what exists today: `want_content` by
title already works for any content type, including book and music (ADR-0082
gave them a `RouteSearch` strategy, just no default profile) — the only
missing piece was a way to SEARCH a metadata provider for a candidate title to
hand it, which is what this ADR adds.

## Decision

### 1. `DiscoveryCandidate.Type` becomes a content-kind string, not a `followed.Type`

It now holds either a `followed.Type` value (a follow-shaped candidate) or a
`works` content type (`movie`, `book`, `music` — a want-shaped candidate). A
new `Source` field names which provider produced it (`tvdb`, `tmdb`,
`openlibrary`, `musicbrainz`), because two providers can mint external ids
from unrelated id spaces and a caller (or the dedup key) needs to tell them
apart. `DiscoveryResult` (the wire shape) gains the same `Source` and a
generic `ExternalID`, and keeps `TVDBID` for backward compatibility — it is
populated only for a `tv_series` candidate, as before.

A candidate's `Type` tells the caller which door to use: the four followed
kinds go to `follow_source`; `movie`/`book`/`music` have no calendar and go to
`want_content` by title+year+content_type instead. `resources.Discover`'s
dedup key becomes `(type, source, external_id)` rather than `(type,
external_id)`, for the same cross-provider-id-space reason.

### 2. TMDB also searches `/search/movie`

`Client.Discover` now fans out to `discoverSeries` (`/search/tv`, unchanged)
and `discoverMovies` (`/search/movie`, new), merging both into one candidate
list. A movie hit maps to `Type: "movie"`, `Source: "tmdb"`; its TMDB id is
carried for display/cross-reference, never for a follow (there is none).

### 3. Open Library and MusicBrainz implement `DiscoverySearcher`

Both were `CapabilityEnrich`-only (ADR-0087): given an already-held book/music
Work, resolve its canonical identity. Discovery is the free-text sibling —
given a query the library has never seen, resolve candidates — and both APIs
already support it (`/search.json?q=`, `/release/?query=`). Unlike `Enrich`,
which cleans a shelf-scraped title before matching, `Discover` runs the query
exactly as the caller gave it — there is no filename noise to strip from
something someone typed on purpose.

### 4. `Registry.DiscoverySearchers` widens from `Route(CapabilityMetadata)` to the union with `Route(CapabilityEnrich)`

The old accessor only ever walked metadata providers, which was correct while
only TVDB/TMDB could discover anything. It stays capability-gated rather than
becoming "every registered provider that happens to implement `Discover`" —
that looser form would let a provider answer discovery queries it never
declared it could (and silently mismatches `providers.Fake`, the test double,
which implements every interface unconditionally regardless of the capability
it was registered under — an indexer-only fake would wrongly start answering
discovery). Gating on `CapabilityMetadata ∪ CapabilityEnrich` is the minimal
widening that admits Open Library/MusicBrainz while keeping the "declared
capability, not incidental interface satisfaction" discipline every other
routing accessor here already uses.

## Consequences

- `discover_content` (MCP) and `POST /api/v1/discover` now return movie, book
  and music candidates alongside TV series, when the corresponding provider is
  configured. A node running only TVDB is unaffected — it still returns
  tv_series only, exactly as before.
- A client (desktop/mobile) that only ever handled `tvdb_id` results should
  read `type` and branch: the four followed kinds still follow; the rest want.
  `TVDBID`'s meaning is unchanged, so nothing that only reads `tvdb_id` breaks.
- **Book and music acquisition remains crude, on purpose, unchanged by this
  ADR.** A discovered book/music candidate is wanted the same way any
  title-only want is: against a profile without a real quality axis for that
  medium (see ADR-0082's `ebook`-shaped profile pattern — accept on source,
  terminal on any bytes). That is still honest, not a regression — the
  vocabulary gap ADR-0082 and ADR-0077 both named is untouched here.
- **What would make us revisit:** the deeper acquisition generalisation
  ADR-0077 flagged (indexers + a quality model that understands bitrate,
  format, edition for music/books) is still not built. This ADR only closes
  the discovery half.
