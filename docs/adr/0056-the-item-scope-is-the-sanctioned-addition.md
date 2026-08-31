# 0056. The Item entity and the item scope are the sanctioned addition, not a retrofit

**Status:** Accepted
**Date:** 2026-08-31
**Milestone:** M12 — Followed Sources / The Archive (Phase 1, slice 1)

## Context

Since Milestone 3, `internal/domain/desired/desired.go` has carried a design
note that reads, in retrospect, like a deferred work order:

> §55's own example is `"content_id": "episode_456"`, and Heyarr cannot
> currently express it. §11 makes the WORK the series, not the episode — an
> edition is a season, and an episode is an Asset, which is a file that exists.
> There is therefore no entity anywhere in the content model for "the fifth
> episode of season two, which I do not have", and a want has nothing to point
> at. […] Inventing one here would be a content-model change smuggled into a
> desired-state issue […] knowing which episodes SHOULD exist is exactly what a
> metadata provider is for […] When one lands it can enumerate expected
> episodes, and **episode scope becomes an addition rather than a retrofit.**

M12 introduces the thing the note was waiting for: a **feed adapter** — a
`CapabilityMetadata` provider (the metadata provider deferred since M1/M3) that
enumerates the items a source emits over time. A *followed source* projects each
enumerated item as a per-item want so the existing acquisition pipeline archives
it. That projection needs a want that points at one episode, and there is still
no entity for an episode that has no bytes yet. This is the moment the note
predicted, and it must be built the way the note prescribed: as an **addition
that lands together**, not a bare desired-state field smuggled in on its own.

## Decision

Add a first-class **Item** to the content spine, a **`ScopeItem`** to
`desired.Scope`, and an **item-completeness fold** to satisfaction — together.

### 1. The Item entity sits between Edition and Asset

```
Work ── Edition ── {Item} ── Asset ── Blob
```

An **Item** is one thing a source emitted — a TV episode, a podcast entry, a
YouTube video, an RSS article — and it **can exist as metadata before any bytes
do**. That byte-less existence is the entire requirement, and it is why an Item
is neither of the two things it is easily mistaken for:

- **Not an Asset.** An Asset is "a file that exists" (`architecture.md`,
  `desired.go`). Modelling an item as an Asset reintroduces the exact gap the
  note names: there would again be nothing to point a want at until the bytes
  arrive.
- **Not an Edition.** Editions are seasons / volumes / releases. Overloading
  them loses the season grouping, and "which episodes should exist" needs its
  own rows regardless. §12 lists `Episode` and `Season` as *distinct* types for
  this reason.

The Item is uniform across all four source types — a TV episode, a podcast
entry, a video, and an article are each "one Item the source emitted, which
resolves to one-or-more Assets when fetched" (the 2160p and the phone copy
remain two Assets of one Item, honouring §61's "never one version per title").
That uniformity is what lets §61's "no separate applications per content type"
actually hold.

**Item shape** (the row lands with the persistence wiring slice; this ADR fixes
its shape): `id`; `work_id` (required — the source's Work); `edition_id`
(nullable — the season / volume grouping, when the source has one); `item_key`
(source-stable identity supplied by the feed adapter: `S02E05`, a podcast GUID,
a YouTube video id, an RSS entry GUID); `title`; `published_at`; `attributes`
JSON. Per-type fields live in `attributes`, never columns — registering a new
source type must not be a migration (`data-model.md`).

### 2. `ScopeItem` on `desired.Scope`

A third scope value, `ScopeItem = "item"`, added to `ScopeWork` and
`ScopeEdition`, with:

- an `ItemID` field on `desired.Item`, set only at item scope;
- `Target()` returning `("item", ItemID)`;
- `Validate()` requiring an `ItemID` at item scope and refusing the other two
  scopes' ids there — the same care the edition arm already takes, so a target
  id never sits unused on a want of a different scope;
- `SameWant` unchanged: it folds over `Target()` and `QualityProfileID`, so two
  profiles of the same episode remain two wants and item scope is part of
  identity for free.

`WorkID` is still required at item scope — an Item belongs to a Work, the
semantic anchor that survives having no bytes.

### 3. Item-scoped satisfaction is the existing evaluator; series completeness is a fold

Content satisfaction for an **item**-scoped want is `EvaluateContent`
**unchanged**: "an acceptable Asset attached to this Item exists". No new
evaluator.

A **source / series**-scoped want (`ScopeWork` over the source's Work) can now
finally mean "every enumerated Item has an acceptable Asset", because the feed
adapter has enumerated them. This is `acquisition.EvaluateCompleteness`, a pure
**fold over the per-item verdicts** — not a new evaluator, a count. It returns
content-axis values only (unknown / not / satisfied; no `converging`, no
`not_applicable`), and:

- **no items enumerated → unknown**, not vacuously satisfied — reporting an
  un-polled source as fully archived is the same lie `EvaluatePlacement` refuses
  for an empty required set;
- **every item satisfied → satisfied**;
- **otherwise → not**, with a stable `Missing` list of the item ids not yet
  held — which is precisely the per-item wants a followed source keeps working.

This is the completeness guarantee the note said was impossible without a
metadata provider, delivered exactly as it said it would be: an addition.

## Consequences

- **The one content-model change of the whole feature is this Item, and it is
  deliberate.** Everything else in M12 reuses what exists: the `Downloader`
  transport, the search beat, the CAS, and M4/M5 replication.
- **The persistence surface widens with the wiring slice, not here.** This slice
  is pure domain (`internal/domain/**`, no `os`/`sql`/persistence — invariant
  2/depguard). The `items` table, the widening of migration 00013's
  `scope IN ('work','edition')` CHECK to include `'item'`, the OpenAPI `scope`
  enum, and a request `item_id` field are added together in the FollowedSource /
  wiring slice so that item scope is REST-reachable and DB-storable in the same
  change that first creates an item-scoped want. Until then the domain can
  *express* item scope and the REST/DB surface deliberately does not yet accept
  it — there is no code path that persists one, so nothing can hit the CHECK.
- **Source-agnostic by construction.** An item-scoped want is an ordinary
  `DesiredItem`; the existing search/evaluate/grab pipeline never learns which
  kind of source emitted it. The follow surface expresses content intent, and
  the system routes to the adapter — the provider layer's "nothing here knows
  which" carried all the way through.
- **What would make us revisit:** if a source type appears whose emitted thing
  is genuinely not one-Item-one-primary-Asset (a live stream, a paginated
  article series), the Item's one-to-many-Assets shape is the first thing to
  re-examine.
