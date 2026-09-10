# 0089. Wanting a series establishes a follow, not a one-off want

**Status:** Accepted (2026-09-10)
**Date:** 2026-09-10
**Milestone:** M12 — Followed Sources / The Archive (Phase 6)

## Context

`want_content` and `follow_source` are two doors onto the library's desired
state, and for a **series** they contradict each other.

- `follow_source` (ADR-0057) is complete for series: given a TMDB/TVDB id it
  creates a `FollowedSource{Type: tv_series}` (followed.go:32), the poll loop
  (`pollsource.go`) routes it to TMDB by `ServesType` (tmdb.go:165), TMDB's
  `Enumerate` walks the seasons and returns one `FeedItem` per episode with its
  air date (tmdb.go:192-240), and each is projected onto an **item-scoped want**
  (`Source.ProjectWant`, followed.go:338) that acquisition then chases. New
  episodes appear on every poll. This machinery works today.
- `want_content` for a series does **none** of that. The MCP handler
  (tools.go:422) collects `{WorkID, Title, ContentType, Year, QualityProfile,
  Monitor}` and no source id; `resources.WantContent` (desired.go:361) creates
  **one work-scoped `desired.Item`** over the series Work and enqueues a single
  reconcile. Nothing enumerates the episodes. Its completeness is not merely
  unmet but **`SatisfactionUnknown`** — "no items have been enumerated for this
  source yet" (satisfaction.go:288) — *forever*, because no path from a want ever
  reaches `Enumerate`.

So "I want Alien Earth" produces a single abstract want that can, at best, match
some one release and can never mean "every episode, and new ones as they air."
The user's question — *should `+want` line up with `follow_source`?* — is the
right one: for a series, **wanting it and subscribing to it are the same intent**,
and the split is an accident of the two doors having grown separately.

This is the near half of the boundary ADR-0077 and ADR-0087 deferred. Those ADRs
were guarding the *provider* side — adding movie/music/book **units** with no
`followed.Type` and no acquisition pipeline. This ADR touches neither: series is
already a `followed.Type` with a working provider (`tv_series` + TMDB) and a
working acquisition path. What is missing is only the **want→follow bridge**.

## Decision

**A work-scoped `want_content` for a series resolves the series' metadata id and
establishes a `FollowedSource` for it, rather than creating a bare work-scoped
want.** The one-off want is subsumed: the follow's poll loop enumerates the
episodes as item-scoped wants and monitors for new ones — the existing machinery,
reached from the want door. Wanting a series *is* following it.

### 1. Only a work-scoped want on a series is bridged

The bridge fires for exactly one shape: `content_type == series`, `Scope ==
ScopeWork` (desired.go:41 — "I want the series"). An **item- or edition-scoped**
want (one episode, one season — desired.go:52/43) is a genuine one-off and stays
a plain want; wanting a single episode must not conscript the whole series into a
subscription. A want for a non-followable type (a movie, a document, a music or
book work) is unchanged — it remains today's one-off want (movies stay ADR-0077's
deferred "want-scoped discovery candidate"; music/books stay ADR-0077/0087).

### 2. The id is resolved, with graceful fallback

`want_content` collects a title (and optional year/work), not a TMDB id. The
bridge resolves it through the `CapabilityMetadata` provider's `Discover`
(tmdb.go:258, `/search/tv` → ranked `DiscoveryCandidate`s carrying the external
id) and takes the **top candidate**. Then it creates
`FollowedSource{Type: tv_series, WorkID: <the series Work>, FeedRef: <resolved
id>, QualityProfileID: <the want's profile>, Monitor: <the want's monitor>,
Backfill: full}` via the one shared `CreateFollowSource` path.

If resolution finds nothing — **no metadata provider configured, or no match** —
the want **falls back to today's bare work-scoped want** rather than erroring.
This is the heyarr stance (a fruitless resolution is never a failure, per
ADR-0057/0085): the feature degrades to current behaviour when it cannot do
better, so a series want still *works* on a node without TMDB, and lights up the
moment one is configured. A wrong top-match is recoverable — unfollow and
re-want — the same reversibility ADR-0088 relies on; a confidence gate is a later
refinement, not v1.

### 3. Backfill is full; the profile and monitor carry over

A want expresses "I want this series" — all of it — so the follow backfills
**full** (BackfillFull, followed.go:89): every aired episode becomes a want, not
only future ones. The want's quality profile becomes the follow's (a series still
*requires* a profile — it has no default, strategy.go / ADR-0082), and the want's
`monitor` becomes the follow's `monitor`.

### 4. Idempotent against an existing follow

If the series Work is already followed, the want does not create a second
`FollowedSource` — it converges on the existing one (asserting the profile/monitor
if they differ) and enqueues an immediate poll. Wanting a series twice, or wanting
one you already follow, is a no-op-shaped reassertion, not a duplicate.

### 5. It crosses ADR-0057's verb boundary, on purpose

ADR-0057 made follow "a standing *subscription*, deliberately a different verb
from `want_content`'s *one-off*." That distinction is right for a one-off work and
wrong for a series: a series is inherently a standing thing, so its one-off want
was the wrong shape from the start. This ADR keeps the two verbs distinct *where
the distinction is real* (a movie, a file, a single episode) and collapses them
*where it is not* (the whole of an ongoing series). The door the user reaches for
— `+want` — now does the right thing for the content type behind it.

## Consequences

- **`+want` on a series now enumerates its episodes and tracks new ones** — the
  visible fix — with no new acquisition or provider code: the bridge reuses the
  whole `follow → Enumerate → ProjectWant → acquire → completeness` spine.
- **Series completeness becomes meaningful.** Instead of `UNKNOWN` forever, a
  wanted series folds the real per-episode wants the poll produced
  (`EvaluateCompleteness`, satisfaction.go:287) — "have I got every episode" finally
  has an answer.
- **A series want with no metadata provider still works** (falls back to a bare
  want) and upgrades itself to a full follow once TMDB is configured and the next
  poll/want fires. No hard dependency, no crash-on-unconfigured.
- **The follow and the want are one record, not two** — the want *is* the follow;
  `list_followed` shows the series, `get_missing_content` shows its per-episode
  wants. No redundant abstract want lingering beside the subscription.
- **`want_content` gains a metadata dependency for series only** — it now calls
  `Discover` on the series path. Other content types are untouched and never call
  it.

## Alternatives considered

- **Keep the series-level want AND spawn a follow (two records).** Rejected: the
  abstract work-scoped want adds nothing the follow's per-episode wants don't
  already carry, and a completeness fold over both is just the follow's fold with a
  vestigial extra row. One intent, one record.
- **Only suggest following (a nudge, no auto).** Rejected as not closing the gap:
  it leaves `+want` on a series a dead-end unless the user takes a second action,
  which is exactly the friction being removed.
- **A unified `acquire()` that routes want-vs-follow by content type.** A cleaner
  long-term API, but a larger refactor of both doors than the gap warrants now;
  this ADR is the content-type-routed behaviour *behind* the existing `want_content`
  door, which is the same routing without the API churn. The unified door stays a
  possible later consolidation.
- **Require the caller to pass the TMDB id (as `follow_source` does).** Rejected:
  `want_content`'s whole ergonomic is "name the thing"; forcing an id there
  recreates `follow_source` under a second name. Resolving the id is the bridge's
  job (§2).
- **Overwrite / re-key nothing — leave `want_content(series)` alone and tell users
  to use `follow_source`.** Rejected: that is the status quo the user correctly
  called a gap; two doors that disagree for the same intent is the defect.

## What would make us revisit

- **Movies** — a movie is a one-off want, not a subscription, so it has no
  `followed.Type`; making `+want` on a movie resolve and acquire needs the
  **want-scoped discovery candidate** ADR-0077 named as its revisit trigger (a
  discovery result that lands as a want, not a follow). That is the natural next
  step and a separate ADR.
- **A confidence gate on id resolution** (mirror ADR-0088) if top-match
  auto-follow proves to pick wrong series often enough to want a "confirm this
  match" surface.
- **Music/book following** — still ADR-0077/0087's deferred path (their
  `followed.Type` + an acquisition pipeline that can grade music/book releases);
  the external ids ADR-0087 now writes are the down payment.
- **A unified `acquire()` door** consolidating want and follow once a third
  content shape (movies) has landed and the routing table is worth centralising.
