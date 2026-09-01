# 0060. A direct release acquires a non-search source (podcast enclosures)

**Status:** Accepted
**Date:** 2026-09-01
**Milestone:** M12 — Followed Sources / The Archive (Phase 2, podcast following)

## Context

Phase 1 (ADR-0056/0057/0058/0059) built the follow pipeline for TV series: a
`poll_source` job enumerates a feed, upserts each episode as a byte-less `Item`,
and projects a per-episode `DesiredItem @ ScopeItem`. From there the EXISTING
acquisition pipeline archives it — search an indexer, evaluate candidates, grab
the winner, verify, ingest. Following = enumerate + project + get out of the way.

Phase 2 adds podcast following, and the plan called it "nearly free": a podcast
episode is audio (an ordinary media file, no content-model change) and the RSS
`<enclosure>` is a direct http(s) URL the existing `KindHTTP` download client
already fetches. Only one thing did not fit the Phase 1 shape, and it is the
whole of this decision.

**A TV episode has no bytes location until an indexer is searched. A podcast
episode's bytes location is exactly what the feed already handed us.** The Phase 1
pipeline routes every item-scoped want through the search job to obtain a
release — and that job is registered only where `CapabilityIndexer` is present
(`worker.go`), because searching with no indexer is nonsense. So a podcast-only
node (a metadata provider + a download client, no indexer) would project the want
and then never run a search for it: the want would rest in MISSING forever, the
quietest failure this feature has. Nothing in Phase 1 sets an item-scoped want's
release source *without* a search.

## Decision

### 1. A feed item that carries its own bytes location is acquired directly, not searched

The neutral `followed.FeedItem` gains a well-known attribute, `AttrEnclosureURL`.
A feed adapter that knows where an item's bytes are (the podcast RSS adapter,
from `<enclosure>`) records the URL there; an adapter that does not (the TVDB
episode adapter) leaves it empty. The poll reads it off the *item*, not off the
source type:

- **enclosure present** → `RecordDirectRelease` writes the URL as the want's
  single, pre-selected release and drives the acquisition to SELECTED; the
  ordinary grab (§64's SELECTED → QUEUED) hands it to `KindHTTP`.
- **enclosure absent** → the want rests in MISSING and the search pipeline finds
  its release, exactly as in Phase 1.

Keying the choice on the item rather than on `followed.Type` keeps the seam
source-agnostic: a later direct-URL source (Phase 4's captured articles) reuses
it with no change at the projection site, and no type switch accretes.

### 2. The direct-release walk is one atomic transaction through the real state machine

`catalog.RecordDirectRelease` inserts the candidate and advances the state in ONE
transaction, walking the genuine edges `search → candidates_found → select`
(never a fabricated MISSING → SELECTED shortcut). This is honest, not a
convenience: in the followed-source sense the poll IS the search — it asked the
feed what it has — it found exactly one candidate (the enclosure the feed named),
and it selected it. The event log carries all three transitions plus a
`search_completed` marked `direct`, the same trace a real search leaves.

One transaction, and guarded to run only from the resting MISSING state, makes it
idempotent (invariant 9): a re-poll re-presents the same episode, finds the want
past idle, and does nothing — no duplicate candidate, no state regression, no
second grab. `DesiredItemForItem` lets a re-poll resolve a want whose creation
raced ahead of its release (CreateDesiredItem returns the duplicate error without
an id), closing the create-then-crash window.

### 3. A direct release is accepted by provenance, not evaluated against the profile

A quality profile RANKS the releases a search returned; a feed's enclosure is the
single authoritative release, with nothing to rank, and a podcast audio file
carries none of §62's video attributes — so gating it on a video-shaped profile
would spuriously reject every episode. The stored candidate is accepted with one
`Reason` (`release/direct-feed`) that says the feed supplied it. The profile is
still required on the want (the model demands it) and still governs any *later*
search of the same work; it simply does not veto a release the feed already
chose, consistent with the feed adapter being the identity authority for its
source.

## Consequences

- Podcast following works on a node with no indexer — the design's claim that the
  abstraction generalises beyond *arr, made executable.
- The content model, the grab, verify, ingest and replication paths are all
  untouched: a podcast episode is a managed Blob like any other and replicates to
  both Full Peers by construction.
- The `podcast` provider kind needs no endpoint and no credential — the feed URL
  is the source's own `FeedRef`, and a public RSS feed authenticates nothing.

## What would make us revisit

- A private podcast feed whose enclosure carries a per-user token is already
  handled (the release source is a `secret.Value`), but a feed that needs an
  *authenticated fetch of the feed itself* would need a credential on the podcast
  kind — a later scheme, deliberately not built now.
- If a future direct-URL source needs the profile to actually gate its releases
  (a captured article filtered by some quality signal), the provenance-accept
  stance here becomes per-type rather than universal — at which point the choice
  moves onto the adapter, where the enclosure attribute already lives.
