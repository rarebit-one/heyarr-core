# 0077. A second metadata provider drops in behind the same interface; non-feed works (movies, music, books) are deferred

**Status:** Accepted
**Date:** 2026-09-07
**Milestone:** M12 — Followed Sources / The Archive

## Context

ADR-0058 defined one `providers.FeedProvider` interface behind
`CapabilityMetadata`, implemented it first with TheTVDB, and promised that
"TMDB is a later, pluggable implementation of the same interface. No caller
names the service." TheTVDB's public v4 registration is currently broken
(thetvdb/v4-api#382), so a node that cannot mint a TVDB key has no way to
discover or follow a TV series at all — discovery (#451) and following both
route only through `CapabilityMetadata`, and TVDB was its only implementation.

We want discovery and following to not depend on one external registration.
The obvious lever is TMDB, which ADR-0058 already named as the alternative. The
question this ADR settles is not *whether* to add TMDB — that was decided — but
how far a metadata provider can reach into heyarr's content model, because TMDB
also indexes **movies**, and the same reach question governs whether **music**
(MusicBrainz) and **books** (Open Library) providers can be added the same way.

## Decision

**Add TMDB as a second `CapabilityMetadata` provider — a drop-in behind the
existing `FeedProvider` and `DiscoverySearcher` interfaces, serving `tv_series`
only. Do NOT add movie, music, or book units: heyarr's follow/discovery model
has no source type for a non-feed work, and adding one is a catalog/acquisition
change, not a provider addition.**

### TMDB is a true drop-in, and needs no domain change

`internal/providers/tmdb` is a sibling of `internal/providers/tvdb`:

- A new `providers.KindTMDB`, whose declared auth scheme is `AuthToken`
  (ADR-0031) — one opaque secret — with `needsCredential` true and
  `needsEndpoint` false (one well-known v3 base URL the client defaults to,
  overridable so tests point it at a fixture server), exactly as TVDB.
- `Discover(query)` maps `/search/tv` hits to the neutral
  `providers.DiscoveryCandidate{Type: tv_series, ExternalID: <tmdb id>, ...}`.
- `Enumerate(ref)` reads `/tv/{id}` for the season list, then walks each
  `/tv/{id}/season/{n}` for its episodes, mapping each to the neutral
  `followed.FeedItem` keyed `S%02dE%02d` — the same key scheme TVDB uses, so the
  poll loop and the projection cannot tell the two services apart.
- Registered in the same constructor `Chain` in the controller and the worker;
  the registry routes to it by capability with nothing else changed, which is
  the addition-not-rename `CapabilityMetadata` was reserved for.

Because a `tv_series` FeedRef is an opaque numeric series id and a deployment
that runs TMDB *instead of* TVDB configures a single `tv_series` adapter,
`feedProviderFor` routes every `tv_series` poll to it unambiguously, and
discover → follow → poll works end to end with no API change — the stated goal.

### Authentication is a v4 bearer token against the v3 endpoints

TMDB accepts either a v3 `api_key` query parameter or a v4 read access token as
an `Authorization: Bearer` header; both are one opaque secret, so both are
`AuthToken`, and how the token goes on the wire is the client's business. This
client sends the **v4 read access token as a bearer header**, deliberately: a
secret in a query parameter travels into the request URL, and a transport error
or a debug log renders that URL verbatim, so a query `api_key` would leak the
first time a request failed. A header keeps the credential out of every URL and
out of the fixture corpus's matched request paths (the replay harness matches
method+path, never a header), which is what lets the synthesised corpus carry no
key — the same property TVDB gets from its bearer token. An operator therefore
supplies a TMDB v4 "API Read Access Token", not the v3 key.

### Exercised only against fixtures

Like every external client here (ADR-0026), the real TMDB client is driven only
against a recorded corpus over `httptest`, never in CI and never with a
committed key. The corpus is `synthesised` from the published v3 contract
because CI has no token, each fixture carrying the justification ADR-0026
requires.

### Movies, music, and books are deferred, not forced

TMDB movies, MusicBrainz release-groups/artists, and Open Library works/authors
were all evaluated and **not** built, because heyarr's model has no place to put
them:

- `providers.DiscoveryCandidate.Type` **is** a `followed.Type`, and the only
  `followed.Type` values are `tv_series`, `podcast`, `youtube_channel` and
  `rss_feed` — all feed-shaped sources whose items are enumerated over time.
- A `FeedProvider` enumerates the items *within* a followed source. A movie is
  not a feed — it is a single one-off want (`want_content` / `desired.Item`),
  not a subscription with a calendar — so it has no `Enumerate` answer and no
  `followed.Type`.
- Music and books *could* be shaped as feeds (a followed artist enumerating
  release-groups; a followed author enumerating works), but making that real is
  a **catalog/acquisition-side** change, not a provider addition: it needs new
  `followed.Type` values and their `Implemented()`/`workContentType` wiring, a
  `follow_source` identity path (today `inferFeed` recognises only `tvdb_id`
  and feed URLs), and — the substantial part — the projected item-scoped wants
  must reach an acquisition pipeline that can search for and grade music and
  book releases (indexers, quality profiles, and §62 attributes are today
  video-shaped). That is milestone-scale work behind the metadata seam, and
  ADR-0058's interface is not where it belongs.

Per the project's scope discipline (§83, CLAUDE.md), a solid TMDB that slots in
cleanly plus an honest boundary is worth more than three half-wired providers.
Movies share the music/book boundary: the missing piece is a **non-feed,
want-scoped discovery candidate** (a discovery result a caller `want`s rather
than `follow`s), plus, for music/books, the acquisition generalisation above.

## Consequences

- **Discovery no longer depends on TheTVDB's registration.** A node configures a
  `tmdb` provider with a v4 read access token and gets discovery and TV-series
  following, unblocking deployments hit by thetvdb/v4-api#382.
- **A discovery result's `tvdb_id` field is now a slight misnomer under TMDB.**
  `resources.discoveryResultFor` surfaces any `tv_series` candidate's external
  id in the `tvdb_id` field, and `follow_source` stores it as an opaque FeedRef;
  this is functionally correct for a single-adapter deployment but the field
  name is TVDB-centric. Renaming it to a neutral `external_id` (or adding a
  `source` discriminator) is a small, separate API change left for when a second
  `tv_series` adapter is run *alongside* TVDB.
- **Running TVDB and TMDB together for `tv_series` is ambiguous.** Both
  `ServesType(tv_series)`, and `feedProviderFor` returns the first in config
  order; a FeedRef carries no provider tag, so a TVDB id could be handed to TMDB.
  This is the pre-existing opaque-FeedRef limitation, acceptable because the
  motivating deployment runs one or the other; a per-source provider selector is
  the fix if simultaneous use is ever wanted.
- **Movies, music and books remain undiscoverable here, by decision.** The exact
  generalisation each needs is recorded above, so the next attempt starts from
  the boundary rather than rediscovering it.
- **What would make us revisit:** a want-scoped discovery candidate (movies), or
  a music/book acquisition path (indexers + quality model for those media). At
  that point a `movie` want-discovery and `music_*` / `book_*` followed types
  become additions behind the same metadata seam this ADR keeps neutral.
