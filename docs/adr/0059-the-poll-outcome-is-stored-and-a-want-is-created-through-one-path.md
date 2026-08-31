# 0059. The poll outcome is stored, and a want is created through one shared path

**Status:** Accepted
**Date:** 2026-08-31
**Milestone:** M12 — Followed Sources / The Archive (Phase 1, slice 4)

## Context

Slices 1–3 (ADR-0056/0057/0058) landed the pure pieces of TV-series following:
the `Item` entity and `desired.ScopeItem`, the `followed.Source` domain with
`ProjectWant`, and the `FeedProvider` interface with a TVDB adapter. None of it
was reachable from a running binary — there was no persistence for a followed
source or an Item, no controller beat to poll, and no worker job to enumerate
and project. This slice wires the pipeline end to end: a `tv_series` source →
TVDB (or a fake) enumerates episodes → each is upserted as a byte-less `Item` →
`ProjectWant` creates a per-episode `DesiredItem @ ScopeItem` → the EXISTING
Torznab-search + grab pipeline archives it. Three decisions here were not
mechanical, and this records them.

## Decision

### 1. The follow beat mirrors the search beat, but the poll OUTCOME is stored, not derived

`internal/controller/followbeat.go` is `searchbeat.go` with three nouns changed:
a 30s tick asks the catalog "which sources are due", enqueues one `poll_source`
job per due source under `followed.PollDedupeKey`, and advances `next_poll_at`
with a compare-and-set so a two-controller race advances the row once. It holds
off when every `CapabilityMetadata` provider is unhealthy, exactly as the search
beat holds off on indexers, and treats "no health rows" as unknown-not-unhealthy.

The one deliberate departure: the search beat DERIVES its backoff exponent
(`fruitless`) at read time from a want's resting acquisition state — "was this
want moved by its last search" has an authoritative answer already sitting in
`acquisition_state`. A followed source has no such resting state: "did the last
poll find a new item" is knowable only from the poll itself. So the poll WORKER
records the outcome (`RecordPollOutcome`): a poll that discovered a new Item
resets the streak to the floor, one that did not backs it off toward the 24h
ceiling. The controller's compare-and-set advance is provisional — it only stops
the next tick re-enqueueing before the worker runs — and the worker's write is
authoritative. Two writers to `next_poll_at`, with the later, better-informed one
winning, is the honest shape when the fact the schedule needs is produced by the
work, not by a resting state.

### 2. A DesiredItem is created through ONE path, used by both the API and the poll worker

`want_content` (the API) and `poll_source` (the worker) must create a want
identically — the row, its resting `acquisition_state`, and both events, in one
transaction (invariant 7). If each wrote it, the two would drift silently, which
is precisely the defect `desired.go` records having been caught by an acceptance
assertion once already. The worker cannot import the API, so the one
implementation is `catalog.CreateDesiredItem`, and `WantContent` was refactored
to call it (resolving the profile and work descriptor first — those stay the
API's business). The insert is plain, so a duplicate `(target, profile)` surfaces
the unique violation: the API maps it to a 409, and the worker treats it as
"already projected" (`IsDuplicateWant`), which is what makes re-running a poll
idempotent (invariant 9).

### 3. Item scope becomes DB-storable via the first table rebuild in this repo

Migration 00013 wrote `desired_items` with `scope IN ('work','edition')` and two
CHECK constraints, and SQLite cannot alter a CHECK. Admitting `'item'` and an
`item_id` therefore needs the twelve-step rebuild — create the new shape, copy,
drop, rename, re-index — which is the first in this tree, so it is spelled out.
Five tables reference `desired_items` with `ON DELETE CASCADE`, so the rebuild
runs with `foreign_keys` OFF (dropping the old table with them ON would fire
every cascade); toggling the pragma is impossible inside a transaction, so the
migration is `-- +goose NO TRANSACTION` and runs on the single writer connection,
where the pragma holds across every statement. The child FKs resolve by NAME, so
after the rename they point at the rebuilt table with their rows intact.

### Consequences and what would revisit them

- **Backfill is a projection filter, not a discovery filter.** Every enumerated
  item is upserted as an `Item`; `from_now` only withholds the WANT for items
  published before the follow. An undated item under `from_now` is left for a
  later poll rather than backfilling an undated catalogue.
- **Projection re-attempts every backfill-passing item each poll**, relying on
  `CreateDesiredItem`'s duplicate refusal, rather than tracking which items are
  already projected. For Phase 1 catalogues (seasons of episodes, polled every
  6h) the cost is negligible; a source with thousands of items would justify a
  projected-set marker, and that is the trigger to add one.
- **Phase 1 routes to the single configured metadata provider.** A source's
  `Type` should choose the adapter, but providers declare a capability, not which
  types they serve, and Phase 1 ships one implementation. Routing a type to one
  of several metadata providers is the seam a later phase fills.
- TVDB is now a real provider `Kind` (`tvdb`), constructed through the injected
  `tvdb.Constructor` in the same Chain as the indexer and download clients.

## Alternatives considered

- **Derive the poll backoff like the search beat does.** Rejected: there is no
  resting state to derive it from without inventing one (a `last_poll_found_new`
  column is the stored outcome by another name).
- **Have the worker duplicate the API's insert.** Rejected as the exact drift
  invariant 7 exists to prevent; the shared `catalog.CreateDesiredItem` is the
  "one implementation, two callers" answer `WantContent`'s own doc argues for.
- **`ALTER TABLE ADD COLUMN item_id` without the CHECK rebuild.** Rejected: the
  column-level `scope IN (...)` and the scope/target CHECK would both still
  reject `'item'`, so the scope would be storable in name only.
