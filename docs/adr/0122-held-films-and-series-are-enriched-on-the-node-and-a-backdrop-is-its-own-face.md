# 0122. Held films and series are enriched on the node, and a backdrop is its own face

**Status:** Proposed
**Date:** 2026-11-02
**Revisits:** ADR-0087 §3 and its named revisit ("a movie/series poster from TMDB
via the same beat"), ADR-0088 §4 ("movies and series are untouched")
**Builds on:** ADR-0075 (browse is a projection), ADR-0105 (a shipped image beats a
fetched one), ADR-0109 (thumbnails are derived blobs)

## Context

ADR-0087 enriched held music and books and said in so many words that enrichment
"does not run on" films and series; ADR-0088 repeated it. A held film therefore
reaches a client with whatever image its release folder happened to ship. Often
that is nothing.

Clients filled the gap themselves. Each one looked the title up against public
services and cached the answer and the image bytes in a local, per-device cache.
Every device repeated the lookup. A transient failure was cached as "no metadata"
for a day. And the discovery read, which already returns a TMDB poster and
backdrop for a title, is read-only: it never stores anything against the held
work. The node is the one place every client already reads, and it was the one
place nothing was stored.

Two shape problems sat under that. The catalogue had one artwork face, the poster
(`artwork`). A wide client slot that wanted a backdrop had to either crop the
poster or fetch from somewhere else. The other problem was the resolver: it ranks
`fanart`/`backdrop` files LAST but still returns them when no poster exists. That
is right for a card, which should show some image rather than none. But it means
a client could not tell "here is the backdrop" from "here is the only image
there is".

## Decision

**TMDB is also an enrich provider: the enrich beat drives held films and series
through it exactly as it drives albums and books through MusicBrainz and Open
Library. A backdrop is a second, independently resolved face of a work, never a
fallback for the poster.**

1. **One beat, one job, one more adapter.** TMDB declares `[metadata, enrich]`.
   Each capability is routed on its own, so this is not the two-routing-modes
   overload ADR-0085 refused. `enrich_work` keeps its per-work dedupe key, its
   backoff schedule and its "a provider miss is never a job failure" stance. The
   under-enriched predicate becomes per content type: a film or series needs a
   poster, a backdrop and a `tmdb` id. Decoration alone (`fanart`, `banner`) is
   not a poster.

2. **Identity is validated, never trusted on rank.** A recorded `tmdb` id is
   used directly. A recorded `imdb` id, or a series' `tvdb` id, resolves through
   `/find`. Only a work with no id is title-searched. A hit must contain the
   query's words on the shared enrich scale, and it must agree on the year to
   within one. Otherwise it is "nothing matched" and the work backs off. A
   stranger's poster is a visible error; a bare card is not.

3. **A film is decorated, never renamed.** The adapter offers no display
   correction, so ADR-0088's rename path does not run on video. The authority's
   overview, genres and year are written as attributes beside the work's own
   identity, never over it. A value some other writer set is kept.

4. **Fetch only what is missing.** The job reads which faces the work already
   resolves and downloads only the absent ones. A re-run downloads nothing the
   node holds, and a fetched image never competes with a shipped one. Fetched
   bytes are adopted into the CAS as ordinary managed assets (ADR-0087 §3).
   Replication, thumbnails (ADR-0109) and GC treat them like any other asset.

5. **A backdrop is its own projection.** A fetched backdrop is filed as
   `backdrop.jpg`, a fetched poster as `cover.jpg`. `backdropPick` selects only
   `backdrop*`/`fanart*` images, shipped before fetched. It is exposed as a
   `backdrop` embed beside `artwork` (card `include=backdrop`, always on the
   detail read) and as `GET /works/{id}/backdrop` with the same `?size=` buckets.
   It answers `null`/404 rather than ever returning the poster. The poster pick is
   unchanged, so no existing client sees a different `artwork`.

## Consequences

- A film or series that any client has never seen arrives with its poster,
  backdrop, overview and ids from the node. The per-device lookup becomes a
  fallback a client can retire.
- A node that configures TMDB for following now also enriches its video. It
  advertises `enrich`, so on a node without Open Library it also claims book
  enrich jobs and backs them off rather than leaving them pending. That is the
  documented "another peer may serve it" path.
- The CAS grows by one poster and one backdrop per held film or series, a few
  hundred KB each, bounded by the library.
- `artwork` is still the one image an old client reads, now present for far more
  films. A client that wants a wide image must ask for `backdrop`; it is not
  substituted.

## What would make us revisit

- **Correcting video identity** when a scene-name parse proves as noisy as the
  book shelves were. That would need an override marker on titles like the music
  artist's, so a person's correction is never reverted.
- **Episode stills and season posters.** This enriches the series work only;
  per-episode images would hang off items, not works.
- **A second video authority** (TVDB artwork) would need the provenance recorded
  here to choose between them.
