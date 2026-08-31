# 0057. A followed source projects items onto wants; the follow beat is the search beat's sibling

**Status:** Accepted
**Date:** 2026-08-31
**Milestone:** M12 — Followed Sources / The Archive (Phase 1, slice 2)

## Context

M12 turns heyarr from a media library into *an archive of our lens of the
internet*: a system that FOLLOWS chosen sources — TV series, podcasts, YouTube
channels, RSS feeds — and pulls every new item they emit into the library,
permanently. TV-series-following (Sonarr-style: grab each episode as it airs) is
Phase 1.

Heyarr already has almost the entire machine this needs. The acquisition lane
(`internal/domain/acquisition`, `internal/domain/desired`, `internal/providers`,
`internal/controller/searchbeat.go`) is a *generic* "this content should exist →
find it → grab it → hash it → land it as an Asset" pipeline that deliberately
knows nothing about content types. ADR-0056 added the one missing content-model
piece: an `Item` (a byte-less thing a source emitted) and `desired.ScopeItem`
(a want that points at one). What is still missing is the thing that decides new
items should exist over *time*. Today a `DesiredItem` is created by a human
(`want_content`). A followed source is just a thing that creates DesiredItems on
heyarr's behalf, forever.

## Decision

**A `FollowedSource` is a standing subscription whose feed adapter enumerates the
items a source emits; each new item is projected as a per-item `DesiredItem` at
`ScopeItem`, and the EXISTING acquisition pipeline archives it untouched.**
Following = enumerate + project wants + get out of the way.

### The three roles, and which is new

| Role | Question | Where it lives | New? |
|---|---|---|---|
| Feed adapter | "what items does this source have now?" | a `CapabilityMetadata` provider | the metadata provider deferred since M1/M3, landing at last |
| Acquisition adapter | "fetch this one item" | the existing `providers.Downloader` | no — Phase 1 adds no transport |
| Control state + projection | the subscription, its policy, its cadence, item→want | `internal/domain/followed` | yes — the one top-level new concept |

`internal/domain/followed` (this slice) holds the pure middle: `Source` (the
subscription value + its invariants), `FeedItem` (what a feed adapter yields),
`Source.ProjectWant(itemID)` (the pure item→`desired.Item` projection), and the
`FeedPoll` schedule. The row that persists a `Source`, the followbeat controller
that polls due sources, and the `poll_source` worker land at the edge in the
wiring slice.

### The follow beat is the search beat's sibling

The follow beat is `searchbeat.go`'s shape, not its code. The search beat decides
*when to look for a want*; the follow beat decides *when to ask a source what it
now has*. It follows the same discipline, verbatim:

- **Controller enqueues, worker runs** (invariant 4 / ADR-0002): the beat is
  control-plane; a feed round-trip is a worker job (`poll_source`, declared in
  `followed/schedule.go` away from its handler, as `acquisition.ReconcileJobType`
  is).
- **The tick is granularity, not cadence.** A ~30s tick asks "which sources are
  DUE"; the cadence a feed host feels is a source's `next_poll_at`, hours–days.
- **`FeedPoll` reuses `acquisition.Schedule`** — its exponential backoff (a
  source that keeps emitting nothing is polled less often, capped, never
  abandoned) and its deterministic FNV spread keyed on the source id (so a batch
  of follows created in one import does not thundering-herd a feed host, and "why
  did this poll at 03:14" has an answer, ADR-0017). Reused rather than
  re-implemented so the determinism is tested in one place.
- **Hold-off on adapter health**, and a `PollDedupeKey` compare-and-set, exactly
  as the search beat holds off on indexer health and dedupes searches
  (ADR-0008): two controllers, or a controller and a forced poll, produce ONE
  poll.

### The output is only DesiredItems

From the moment an item is projected it is an ordinary want walking §64's state
machine (MISSING → SEARCHING → … → CONTENT_SATISFIED). There is no parallel
pipeline and no content-specific job system (§61). A projected want is a plain
`desired.Item`; nothing about it records that a source projected it — which is
what keeps the pipeline source-agnostic.

### Source-agnostic, and follow is not a one-off

The follow surface expresses CONTENT INTENT — "follow this series" — and the
system infers `Type` and routes to the adapter. `Type` is inferred and *stored*
so the poll loop knows which adapter to ask; it is never a knob a caller turns to
pick an adapter, exactly as the provider registry keeps "which provider answers"
out of the caller's hands. And a `FollowedSource` is a standing *subscription*,
deliberately a different verb from `want_content`'s *one-off* "get this once" —
both drive the same downstream machinery.

## Consequences

- **Replication is a free win.** Every archived item lands as a managed Blob, so
  the existing M4/M5 `Diff` → `replicate_blob` → pull-and-re-verify path carries
  it to both Full Peers with no new code — the reason to build this inside heyarr
  rather than as a Sonarr/ArchiveBox stack beside it.
- **The later phases are cheap.** Podcast (Phase 2) reuses `KindHTTP`; YouTube
  (Phase 3) adds only a yt-dlp `Downloader`; RSS (Phase 4) adds the web-capture
  content decision. Each is a feed adapter plus a choice of existing transport.
  `Type.Implemented()` gates the unwired ones so a subscription that would never
  be polled is refused loudly at creation rather than sitting silent.
- **This slice is pure domain** (invariant 2 / depguard): the persistence row,
  the controller beat, the worker handler, and the MCP + REST surfaces (which
  must share one `resources` operation layer so both agents and the heyarr-mobile
  app can follow over HTTP) land in the wiring slice.
- **What would make us revisit:** if a source's cadence genuinely needed
  per-source tuning (a tracker-account-style catastrophic cost the fixed 6h–24h
  band cannot express), `FeedPoll` would grow from a constant policy into stored
  per-source cadence — the same knob the search schedule has so far resisted for
  the same reason.
