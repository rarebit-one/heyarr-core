# 0087. Music and book Works are enriched from a keyed lookup, decoupled from following

**Status:** Accepted (2026-09-09)
**Date:** 2026-09-09
**Milestone:** M12 — Followed Sources / The Archive (Phase 6, catalogue enrichment)

## Context

The library holds music and books — an album is a Work (`content_type = "music"`,
the artist in `attributes.artist`, the format an Edition), a book is a Work
(`content_type = "book"`, the author in `attributes.author`). They were
identified from their filenames at ingest (`internal/domain/identification`),
and that is *all* heyarr knows about them: a title, maybe a year, whatever the
path spelled. They have no canonical identity (a MusicBrainz release id, an Open
Library id), no description, and — the thing most visibly missing — **no cover
art** unless a `cover.jpg` happened to ship in the folder.

The want is ordinary: album covers and book covers, and the metadata behind
them. Everything needed to *serve* a cover already exists — `browse.go`'s
`artworkPick` resolves "the" cover for a Work by walking its editions for a
`role='artwork'` asset (`artworkRank` prefers poster/cover/folder), and
`GET /works/{id}/artwork` / the `include=artwork` card field / MCP
`browse_library`'s `include_artwork` all serve it. The moment a Work has an
artwork asset with a blob, every consumer shows it, with no change. What is
missing is anything that *produces* that asset from the network.

### Why this is not "just add a metadata provider"

ADR-0077 evaluated exactly this — MusicBrainz and Open Library — and deferred it,
recording the boundary precisely: *"Do NOT add movie, music, or book units:
heyarr's follow/discovery model has no source type for a non-feed work, and
adding one is a catalog/acquisition change, not a provider addition,"* and named
the revisit as *"a music/book acquisition path."* This ADR is that revisit — but
it crosses a **different, smaller** part of the boundary than ADR-0077 was
guarding, and naming which part is the whole of the design.

ADR-0077 was guarding **following**: subscribing to an artist or author so heyarr
*acquires new releases* over time. That genuinely needs everything ADR-0077
lists — a music/book `followed.Type`, a `follow_source` identity path, and an
acquisition pipeline that can search indexers for and grade a music or book
release. None of that exists, and this ADR does not build it. Following music and
books stays deferred.

What the want actually asks for is **enrichment of Works already held**: take the
album and the book that are *already in the library* and give them their
canonical ids and their cover. That needs none of the following machinery — no
`followed.Type`, no subscription, no indexer search, no acquisition grading —
because there is nothing to *acquire*: the album's audio and the book's file are
already here. It needs one thing heyarr has never had, and that is the crux.

### The one thing that does not exist: an enrichment trigger

Every field on a Work is written **once, from a filename parse, at ingest**
(`catalog.resolveWork`/`resolveEdition` from an `identification.Candidate`).
Nothing in heyarr ever takes an *existing* Work and fills anything in from an
external source. The `CapabilityMetadata` providers (TVDB, TMDB) are consumed by
one caller only — the poll loop, which uses them to project *new items* for a
*followed source* (`pollsource.go` `feedProviderFor`, routed by
`ServesType(followed.Type)`); they never write back to a library Work. And
`external_ids` (ADR-0050) — the obvious home for a MusicBrainz id — is a
read-only projection today: every reader is production, every *writer* is a test
harness. There is no path that says "here is a Work; go find out more about it."

So this feature is not a provider drop-in behind an existing seam (that seam,
`ServesType(followed.Type)`, cannot even be pointed at a Work — it routes by
subscription type, and there is no music/book subscription type). It is a new
**enrichment** capability and the trigger that drives it.

## Decision

**A new `CapabilityEnrich` provider answers "what is this Work, and where is its
cover" keyed on the Work itself; an enrichment beat drives held music/book Works
through it, writing their external ids and fetching their cover as an ordinary
`role='artwork'` asset the existing resolver already serves.** Following music and
books remains ADR-0077's deferred, separate concern.

### 1. Scope is enrichment of held Works, never following

The unit of work is a Work already in the library. There is no subscription, no
`followed.Type`, no indexer, no acquisition grading — the content exists; only its
*description* is being filled in. This is deliberately the half of ADR-0077's
boundary that carries none of its cost, and it is why this can ship without the
catalog/acquisition changes ADR-0077 requires for following. "Follow an artist /
author and archive their new releases" is out of scope and stays deferred to
ADR-0077's named path.

### 2. `CapabilityEnrich` — a Work-keyed lookup, not a feed

A new capability (`internal/providers/capability.go`, one more const + one more
routing accessor — the open-set discipline ADR-0085 used for `CapabilitySubtitle`)
because an enrichment provider answers a question no existing capability does:
*given a Work — its content type and the identity attributes ingest parsed
(artist+album, author+title) — what is its canonical id, and where is its cover?*
It is NOT `CapabilityMetadata`: that capability is routed by
`ServesType(followed.Type)` for the poll loop, and enrichment keys on a Work's
content type, not a subscription type — overloading one capability with two
routing modes is the collapse ADR-0085 refused when it gave subtitles their own
capability rather than folding them into metadata.

```
type EnrichProvider interface {
    Provider
    ServesContentType(ct string) bool            // "music" | "book"
    Enrich(ctx, EnrichQuery) (EnrichResult, bool, error)
}
type EnrichQuery  { ContentType, Title string; Artist, Album, Author string; Year int; ExternalIDs map[string]string }
type EnrichResult { ExternalIDs map[string]string; CoverURL secret.Value; Overview string /* neutral, no transport */ }
```

Values in, values out — the line every provider draws (ADR-0026 fixtures test it,
never live). Two adapters implement it:

- **MusicBrainz** (`internal/providers/musicbrainz`) — music. Looks a release up
  by artist+album (+year), returns the MBID and, via the linked **Cover Art
  Archive**, the cover URL. **Keyless**, but it enforces ~1 req/s and *requires* a
  descriptive `User-Agent`; the client reuses the proactive rate limiter built for
  OpenSubtitles (ADR-0085) — the second user of that infrastructure, as predicted.
- **Open Library** (`internal/providers/openlibrary`) — books. Looks a work up by
  author+title, returns the OLID and the cover URL (`covers.openlibrary.org`).
  **Keyless.**

Both declare `AuthNone` (credential.go) — no operator secret, so, unlike the
video and subtitle providers, **no 1Password credential and no sops secret**;
configuring them is an endpoint (defaulted) and a `User-Agent`, nothing more.

### 3. The enrichment trigger: an enrich beat and an `enrich_work` job

The genuinely new machinery. A control-plane beat (the sibling of the search,
follow, subtitle and download beats — controller enqueues, worker runs, invariant
4) finds held music/book Works that are **under-enriched** — no
`role='artwork'` asset resolvable for the Work (`artworkPick` empty) and/or no
external id from an enrich source — and enqueues one `enrich_work` job per due
Work, gated on `CapabilityEnrich`, on a backoff cadence (a Work nobody's provider
knows is retried less often, capped, never abandoned — the follow-poll stance,
ADR-0057). The worker:

1. reads the Work's content type + identity attributes, routes to the
   `EnrichProvider` whose `ServesContentType` matches, and calls `Enrich`;
2. **writes `external_ids`** for the Work (`source='musicbrainz'|'openlibrary'`,
   `entity_type='work'`) — the *first production writer* of that table (ADR-0050
   built the read side and the shape for exactly this);
3. if a cover URL came back, fetches it with the existing `KindHTTP` download
   client and attaches the bytes as a `role='artwork'`, `image/*` **managed
   asset** (ADR-0020) via `RecordFetchedArtwork` — the generalisation of
   ADR-0084/0085's `recordSubtitleAsset` (idempotent on `(edition, blob, role)`,
   self-peer replica, `TypeAssetCreated` event), which is already artwork-agnostic
   apart from its two subtitle constants.

Then **every consumer serves the cover with no change** — `artworkPick` /
`GET /works/{id}/artwork` / the card and search `include=artwork` / MCP
`include_artwork` — exactly the "indistinguishable in shape from what a release
shipped, so the resolver serves it with zero consumer changes" property ADR-0084
established for extracted subtitles.

### 4. Enrichment ADDS; it does not overwrite identity

The provider's answer is *additive*: it writes external ids, attaches a cover, and
may store a description in `attributes` (`overview`, `genre`). It does **not**
overwrite the Work's `title`, `year`, `artist`/`author` — those were identified
locally and are the identity the `work_key`/unique index and every existing
reference depend on. A provider that returns a different title is reporting a
match with different spelling, not a correction to apply blindly: overwriting
would let a wrong fuzzy match rename a correctly-identified album. Identity stays
the ingest heuristic's; enrichment decorates it. (A confident-mismatch path — the
provider disagrees strongly — is a later, careful concern, not this ADR's.)

### 5. A cover attaches at an Edition, and resolves at the Work

Assets hang off `edition_id`, and `artworkPick` resolves a Work's cover by walking
*all* its editions — so a cover attached to any one Edition of the Work resolves
for the whole Work. `RecordFetchedArtwork` attaches it to the Work's
representative Edition (the one carrying the primary asset; for a multi-format
album — FLAC and MP3 Editions of one album Work — either serves the Work's cover
identically, so the primary Edition is chosen and not duplicated across formats).
This matches how a shipped `folder.jpg` already behaves.

### 6. It is a job, not a want (contrast ADR-0085)

A subtitle is *missing content* for an item that should exist — a desired-state
question, so ADR-0085 modelled it as an aspect-scoped want walking the acquisition
machine. A cover is different: the album and the book **are already held**; their
canonical id and their cover image are a *missing facet of content we have*, not
content we lack. So enrichment is a **job over an existing Work** — the shape of
ADR-0084's `extract_subtitles` (turn what you have into an asset the resolver
serves), not ADR-0085's want (acquire what you do not have). It carries no quality
profile, no acquisition state, no placement axis: a Work is enriched or it is not
yet. The beat's backoff is the only state it needs.

## Consequences

- **Covers and ids appear on music and books already in the library, with no new
  consumer code** — the serving path is untouched; this feature only fills the
  producer side ADR-0077 left empty.
- **`external_ids` gains its first production writer.** A MusicBrainz/Open Library
  id on a Work is then readable by `get_external_ids` and the REST detail views
  (ADR-0050), and — usefully — becomes the strong key a *future* music/book
  subtitle-style or acquisition feature can match on, the same way subtitle fetch
  matches imdb/tmdb (ADR-0085).
- **No new secret surface.** MusicBrainz, Cover Art Archive and Open Library are
  keyless — so no 1Password items, no sops secrets, no per-host credential (the
  video and subtitle providers' whole credential story does not apply). The rate
  limiter is the only new infra, and it already exists (ADR-0085).
- **Following music and books is still not possible** — this enriches held Works;
  it does not subscribe to an artist or author. That remains ADR-0077's deferred
  path (a music/book `followed.Type` + an acquisition pipeline), and this ADR
  deliberately does not start it.
- **A Work with only a filename and no provider match stays bare** — enrichment
  that finds nothing backs off and leaves the Work as identified, exactly as a
  fruitless subtitle fetch leaves a want (ADR-0085); it is never an error.

## Alternatives considered

- **Fold enrichment into `CapabilityMetadata`.** Rejected: that capability is
  routed by `ServesType(followed.Type)` for the poll loop, and enrichment routes
  by a Work's content type — two routing modes on one capability is the overload
  ADR-0085 refused. A distinct `CapabilityEnrich` keeps each capability answering
  one question.
- **Model a cover as an `aspect='artwork'` want (mirror ADR-0085).** Rejected: a
  want is a desired-state claim about content that should *exist and be acquired*;
  a cover for a held album is enrichment of existing content, and the want model's
  item/edition scope, quality profile and placement axis are all meaningless for
  it. A Work-scoped enrichment job is the honest shape (§6).
- **Overwrite Work fields from the provider.** Rejected (§4): identity is the
  ingest heuristic's, and a fuzzy provider match must not silently rename a Work.
- **Do it as part of following (ADR-0077's full path).** Rejected as scope:
  following is a large, separate build; enrichment delivers the covers-and-metadata
  want on its own, and leaves the ids that would *help* a later following feature.

## What would make us revisit

- **Following music and books** — an artist/author subscription that acquires new
  releases — is the rest of ADR-0077's deferred path and a separate ADR; the
  external ids this feature writes are a down payment on it.
- **Enrichment overwriting identity** when a provider match is confident enough to
  correct a bad local identification, rather than only adding to it (§4).
- **Enriching other content types** (a movie/series poster from TMDB via the same
  beat) — the enrich trigger is content-type-agnostic; only the adapters are typed.
- **A person-level entity** (artist, author) — today they are attribute
  projections (`list_artists`/`list_authors` group Works by a string); an enrich
  source that returns a stable artist/author id is what would justify promoting
  them to entities.
