# 0064. A followed source is routed to its feed adapter by type

**Status:** Accepted
**Date:** 2026-09-01
**Milestone:** M12 — Followed Sources / The Archive (#415, closing the phase gap)

## Context

Phases 1–4 (ADR-0057…0063) built four feed adapters — TVDB (tv_series), podcast,
YouTube (youtube_channel), webfeed (rss_feed) — each complete and tested behind
the `FeedProvider`/`Downloader` seam. But following a YouTube channel or an
article feed was not reachable end to end through the follow API, for two coupled
reasons the earlier phases deliberately left as a later step:

1. **Type inference collapsed everything non-TVDB to podcast.** `inferFeed`
   classified a source's type from its identity, but only tv_series (a TVDB id or
   thetvdb.com URL) was recognised; every other http(s) URL fell through to
   `podcast`. A YouTube feed URL, or an article RSS feed, was silently stored as a
   podcast.

2. **The poll routed to `feeds[0]`, not by type.** `worker.feedProviderFor`
   returned the first `CapabilityMetadata` provider in routing order rather than
   the one that serves the source's type. With a single adapter configured that is
   harmless; with two, a podcast source could be enumerated through the TVDB
   adapter — which answers nothing and presents as a dead feed.

Both had to be fixed together: inference sets the stored `Type`, and the poll must
honour it.

## Decision

### 1. A feed adapter declares which source type it serves

`providers.FeedProvider` gains `ServesType(followed.Type) bool`. Each adapter
answers for its own type (TVDB → tv_series, podcast → podcast, youtube →
youtube_channel, webfeed → rss_feed). The poll's `feedProviderFor(reg, type)`
returns the adapter that serves the source's type, and a type no configured
adapter serves is an ERROR the poll sees (a misrouted job, or an adapter removed
from config) rather than an empty enumeration through the wrong adapter.

The mapping lives on the adapter, not inferred from its provider Kind, for the
same reason `Enumerate` does: the registry holds providers as the interface, and
the adapter is the thing that knows its own source shape. The test `Fake` serves
any type by default, so the many single-adapter follow tests route to it
unchanged; a routing test configures `ServingTypes` to prove the poll reaches the
RIGHT adapter when two are present.

### 2. Type is inferred where the URL allows it, and named where it cannot

`inferFeed` now recognises a YouTube channel from the URL alone — a
`youtube.com/feeds/videos.xml?channel_id=…` feed URL or a `/channel/UC…` URL both
carry the channel id, and the stored `feed_ref` is normalised to the canonical
feed URL either way. An `@handle` or custom URL has no id without a lookup and is
refused with a pointer, exactly as a TVDB slug URL is.

A podcast RSS feed and an article RSS feed are **the same shape at the URL**, so
inference cannot separate them. Rather than sniff the feed's bytes at follow time
(a network call, and a guess), the request gains an optional `type`: when given
it is authoritative and must be an implemented type consistent with the identity
(not `podcast` for a `tvdb_id`); when empty a plain feed URL stays a podcast, the
backward-compatible default. This keeps the follow API source-agnostic in spirit
(#396) — the caller still never names a provider — while giving it the one bit of
identity the URL genuinely cannot carry.

`workContentType` maps the two new types to their work content types (a channel's
videos under a `video` work, an article feed's entries under a `document` work),
so a follow and a later scan converge on one work as they already do for series
and podcasts.

## Consequences

- Following a YouTube channel or an RSS/article feed now works end to end through
  both the REST and MCP doors, exercising the Phase 3/4 adapters that until now
  had no caller past their unit tests.
- A deployment can configure more than one metadata adapter and each source is
  polled through the right one.
- `document`/`video` are stored as work content types with no migration —
  `works.content_type` is free-text and the follow path sets it directly, as it
  already does for `podcast`/`series` (ADR-0063 confirmed this needs no vocabulary
  change).

## What would make us revisit

- **Discovering brand-new works not yet in the library** — resolving a YouTube
  `@handle`, or a channel/feed by name — is a separate metadata-search mode
  (decision 2), deliberately not built here: this routes an *explicitly-identified*
  source of a known type.
- If a caller should be able to follow an article feed WITHOUT naming the type, a
  content sniff (does the feed carry audio enclosures?) at follow time would
  replace the podcast default — at the cost of a network call inference has so far
  avoided.
- A second adapter for the same type (a TMDB beside TVDB) would make `ServesType`
  match more than one provider; the first-match routing here becomes a preference
  order at that point, declared where the adapters are registered.
