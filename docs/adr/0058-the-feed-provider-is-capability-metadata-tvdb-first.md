# 0058. The feed adapter is a CapabilityMetadata provider; TVDB is the first, TMDB is pluggable

**Status:** Accepted
**Date:** 2026-08-31
**Milestone:** M12 — Followed Sources / The Archive (Phase 1, slice 3)

## Context

ADR-0057 established that a followed source's *feed adapter* answers "what items
does this source have now?", and that this is the metadata provider heyarr has
deferred since Milestone 3. `providers.CapabilityMetadata` has existed since then
"with nothing implementing it", declared so the first metadata provider would be
an addition to configuration rather than a change to the registry.

Phase 1 needs the first implementation: a TV-series adapter that enumerates a
series' episodes and air dates — the calendar Sonarr-style episode-following
turns on ("which episodes should exist, and when did or will they air"). Two
services can supply that calendar, and which to use was a flagged user decision:
**TVDB** vs **TMDB**.

## Decision

**Define one `providers.FeedProvider` interface behind `CapabilityMetadata`, and
implement it first with TVDB. TMDB is a later, pluggable implementation of the
same interface. No caller names the service.**

### The interface returns neutral values

```go
type FeedProvider interface {
    providers.Provider
    Enumerate(ctx, ref string) ([]followed.FeedItem, error)
}
```

`Enumerate` takes the source-stable `ref` a `FollowedSource` carries (a TVDB
series id; a podcast feed URL; a channel id — opaque to the caller, parsed by the
adapter) and returns the neutral `followed.FeedItem`, never a TVDB- or
TMDB-shaped value. This is what keeps the poll loop and the projection
source-agnostic AND provider-agnostic: the source's `Type` decides what an item
means, and a second metadata service slots in behind the interface without the
loop changing. It returns an error (not a Health) because a failed enumeration
is a call failure the poll loop must see and retry — folding "unreachable" into
an empty slice would report a source as having emitted nothing.

### TVDB first

TVDB is the *arr-ecosystem standard for accurate season/episode numbering and
air dates, which is precisely the feed adapter's whole job. TMDB has broader
general metadata and friendlier API terms and is a reasonable alternative or
augment; it becomes a second `FeedProvider` implementation when a later phase
wants it. Neither is hardcoded into the control plane — a followed source names
its metadata provider by configuration, and `internal/providers/tvdb` couples
nothing outside itself to TVDB's JSON.

### Exercised only against fixtures

TVDB is an external service reached with a credential, so per ADR-0026 the real
client is driven only against a recorded corpus over `httptest`, never in CI, and
never with a committed key. The corpus for a service without a public no-auth
endpoint is `synthesised` from the published v4 contract, each fixture carrying
the justification ADR-0026 requires; it is regenerated against a real key with
`scripts/capture-fixtures.sh`. The API key is supplied at construction from a
credential or an environment reference at the edge.

## Consequences

- **The registry routes to it for free.** A TVDB provider advertises
  `CapabilityMetadata`, which is exactly the addition-not-rename the capability
  was reserved for; nothing in the registry changed to admit it.
- **This slice ships the interface and the adapter, not the wiring.** Registering
  a TVDB *kind* in provider configuration, and the followbeat calling
  `Enumerate`, land in the end-to-end wiring slice — the adapter is proven in
  isolation first, as every external client here is.
- **TMDB is a drop-in, not a rewrite.** The neutral return type and the
  by-configuration selection are what make that true; if they were not, the
  second provider would be the moment this became TVDB-shaped.
- **What would make us revisit:** if episode identity needed more than
  `(season, episode)` — an absolute-numbering or a multi-provider reconciliation
  problem — the `FeedItem.Key` scheme, not the interface, is where it would be
  reworked.
