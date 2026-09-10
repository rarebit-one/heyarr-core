# 0093. An episode want searches and matches by season and episode

**Status:** Proposed (2026-09-11)
**Date:** 2026-09-11
**Milestone:** M12 — Followed Sources / The Archive (episode-scoped acquisition)

## Context

A followed series projects one item-scoped want PER EPISODE. ADR-0056 placed the
Item between Edition and Asset so a want could point at one episode before its
bytes exist, and a feed adapter enumerates those items: `items.item_key` is
`S01E01`, and `items.attributes` carries `{season: "1", episode: "1",
tmdb_episode_id: …}`. ADR-0089 establishes the follow that produces them.

Observed live on a running node, 2026-09-10, for a season-one-episode-one want:
the search built a candidate list of **whole-season packs across every season** —
`Slow Horses S01 (1080p…)`, `S02…`, `Season 3 S03 (2160p…)`, `S04`, `S05` — every
one marked `accepted`, and heyarr **selected the S03 2160p pack for the S01E01
want**. An S03 pack cannot contain S01E01. The matching is season-blind end to
end: it neither scopes the search to the target season/episode nor filters the
candidates to those that could contain the wanted episode, and it then ranks by
quality alone, so the highest-resolution pack of the wrong season wins.

Three sites, on `origin/main`, together produce this:

1. **The search term is the series title, with no season or episode.**
   `worker.SearchHandler` (`internal/worker/search.go:115`) issues
   `providers.Query{Title: sc.Title, Year: sc.Year, ContentType: sc.ContentType,
   Limit: …}`. `providers.Query` (`internal/providers/provider.go:139`) has only
   `Title`, `Year`, `ContentType`, `Limit` — there is no `Season` or `Episode`
   field to carry. `catalog.SearchContextFor`
   (`internal/persistence/catalog/candidates.go:385`) reads only `w.title,
   w.year, w.content_type` from the Work; it never joins `items` to read the
   want's `item_key` or its season/episode attributes, even though
   `DesiredItemTarget` (`internal/persistence/catalog/desireditems.go:182`) knows
   the want carries an `item_id`. `prowlarrClient.Search`
   (`internal/indexers/prowlarr.go:198`) therefore sets `query = q.Title`
   (`+ year`) and constrains only the category. So the S01E01 want asks Prowlarr
   for `Slow Horses` and is handed every release for the series — every season's
   packs included.

2. **Nothing gates a candidate by season or episode.**
   `acquisition.Evaluate` / `EvaluateAll`
   (`internal/domain/acquisition/candidate.go:202` / `:444`) score each candidate
   against the quality `policy.Profile` and nothing else. Season and episode are
   not policy attributes and there is no separate containment check. A release's
   `Title` is, by explicit design, "never parsed here" (`candidate.go:68`) — the
   attributes come from the provider (ADR-0091). So the S03 2160p pack passes
   every quality gate (`Accepted: true`), outscores the lower-resolution packs,
   and `Best` returns it for the S01E01 want. The parser that *could* tell —
   `internal/domain/identification/series.go` (`reSxxExx`, `splitSeasonFolder`,
   `matchSeriesSeasonDir`) already reads season, episode and season-pack shape off
   a title — is wired only into library scan and ingest, never into candidate
   matching. ADR-0091 already named this exact hazard: "the
   season-pack-vs-episode problem (a 4 GB `S01` pack matching six episode wants)".

3. **A season pack cannot satisfy its episode wants today anyway.**
   Even had a *correct* S01 pack been selected, `verifyArtifact`
   (`internal/worker/ingestacquisition.go:240`) refuses a directory outright — "a
   multi-file release is not ingestable in this milestone" — so a season pack
   (inherently multi-file) is blocked as `BlockIngestFailed` and every episode
   want stays unsatisfied. And the item→asset link ADR-0086 introduced,
   `SetAssetItem(res.AssetID, itemID)` (`ingestacquisition.go:326`), links the one
   asset the pipeline produced to the one item the want asserts; there is no
   fan-out from a pack's many files to many items. So the wrong-season selection
   is not even a near miss: the release downloads, fails verification as a
   directory, is blocked, the want returns to rest, and the next search finds the
   same packs minus the blocked one — a slow, bandwidth-spending loop.

The crux is (2): there is no season/episode gate anywhere between the indexer's
answer and the selection. (1) makes the candidate list needlessly noisy; (3)
means the thing that gets selected could not have worked regardless. All three
have to be named for the fix to be honest, but the gate is what stops the
observed bug.

## Decision

### 1. The query carries the target season and episode, and uses them

`providers.Query` gains `Season` and `Episode`, each with a sentinel for "not an
episode search" (`0` is a legal season — Specials — so the absent value must be
distinct; a pointer or an explicit `-1`, decided at implementation). For an
item-scoped want on a series, `SearchContextFor` joins `items` and reads the
season/episode from `item_key` / `items.attributes`, and populates them; for a
work- or edition-scoped want, and for every non-series content type, they stay
absent and the query is exactly today's.

An indexer that understands a structured TV search (Torznab `tvsearch` with
`season` and `ep`, keyed by `tvdbid` where available) is asked for the episode
directly. Where only free-text is available, the term becomes `Title SxxEyy`
(and, for a deliberate pack search, `Title Sxx`). This narrows the candidate list
at the source; it is a courtesy to the indexer and a reduction in noise, not the
correctness boundary. The gate in §2 is the correctness boundary, because an
indexer's free-text match is not something heyarr controls or can trust.

### 2. A candidate must plausibly CONTAIN the wanted episode — a match gate, before scoring

A new containment gate runs on the returned candidates *before* §63's quality
evaluation, in the same position and spirit as `excludeBlocked`
(`search.go:201`): it decides membership in the running, not quality. It reuses
the scanner's parser (`internal/domain/identification/series.go`) — one reading of
a title, not a second — to derive each candidate's series/season/episode/pack
shape, marked derived per ADR-0091 so the explanation stays honest about where the
fact came from.

For an episode want (season *S*, episode *E*), a candidate is kept only if it
plausibly contains that episode:

- an episode release for **exactly** *SxxEyy* (including a multi-episode file
  whose range covers *E*); or
- a **season pack for season *S*** (`splitSeasonFolder` / a bare `Sxx`).

A candidate for a **different season**, a **different episode**, or a
**different series** is rejected — with a durable, machine-coded reason, exactly
as a quality rejection is (§60, ADR-0091's honesty property). This is a *match*
gate and deliberately NOT a `policy.Profile` accept rule: season and episode are
questions of identity and containment, not of quality, and folding them into the
profile would conflate "this is the wrong episode" with "this is the wrong
resolution" — two rejections an operator must be able to tell apart. A candidate
whose season/episode cannot be determined at all is rejected for an episode want
rather than assumed to match, the same safe direction `Evaluate` takes for an
undetermined accept gate (`candidate.go:222`).

Only the survivors reach `EvaluateAll`. Quality still decides among things that
could actually contain the episode; it never again decides *across* seasons.

### 3. A season pack is a legitimate candidate, ranked below an episode release, and satisfies every episode want in its season — downloaded once

Season packs are frequently the only availability, so rejecting them wholesale
would trade one bug for a worse one. The position is:

1. **An episode release for the exact episode is preferred over a season pack**
   when both survive the gate and both are acceptable. A pack drags in a season's
   worth of bytes to satisfy one want; the exact-episode release is the smaller,
   more precise acquisition. This is a ranking preference among gate-survivors,
   expressed so §63's total order stays deterministic (`candidate.go:444`) — a
   pack-vs-episode tiebreak below score, not a new gate.

2. **Acquiring a season pack must satisfy EVERY episode want it contains, and the
   pack must download once.** This is the honest completion of ADR-0086's
   item→asset edge and requires three things this ADR commits to as the design,
   to be built as its own change:
   - **Multi-file ingest** — lift the directory refusal at
     `ingestacquisition.go:240`. The refusal is explicitly a
     "not-in-this-milestone" placeholder, and it is now on the critical path.
   - **Per-file item mapping** — extend ADR-0086's single
     `SetAssetItem(assetID, itemID)` to a fan-out: each file in the pack is parsed
     for its `SxxEyy`, matched to the enumerated item whose `item_key` it equals,
     and its asset linked to that item. The item is still an asserted fact
     (the enumerated `item_key`), resolved against the file's derived season/
     episode — not identity re-derived from a filename, which ADR-0086 refused.
   - **One transfer, one blob, many satisfied wants.** Six episode wants that each
     select the same pack must not fetch it six times. The pack is one release,
     one download-client transfer, one CAS blob; per-want satisfaction is a
     reconciliation question (`EvaluateContent`, `satisfaction.go:82`) over the
     shared assets the single ingest produced, exactly as replication dedups by
     blob. The grab is deduplicated by the pack's blob/source, not by the want.

3. **Interim, correct-but-smaller.** §1 and §2 alone stop the observed bug: the
   S01E01 want stops even considering an S03 pack, and prefers an S01E01 release.
   Until pack fan-out (§3.2) lands, a season pack that survives the gate is still
   only ingestable as the single-file case allows — so during the interim the
   *matcher* prefers episode releases and a pack for the target season is a
   last-resort candidate whose ingest limitation is a known, tracked gap, not a
   silent wrong-season acquisition. The gate is the part that ships first because
   it is the part that is actively wrong today.

## Consequences

- The observed bug is closed: an episode want can no longer select a release from
  a season it does not want. A wrong-season or wrong-episode candidate is rejected
  with a durable reason an operator can read, alongside the quality reasons, so
  "why did it grab an S03 pack for S01E01" has an answer that names the rule.
- `providers.Query` gains meaning it can express and every indexer can ignore
  safely — absent season/episode is exactly today's behaviour, so movies, music,
  books and work-scoped series wants are unchanged.
- The scanner's parser earns a second caller. The same reading of `SxxEyy` that
  identifies a file on disk now identifies a release in a search result, and the
  two cannot drift — the same one-vocabulary argument ADR-0091 made for quality.
- The season-pack path becomes honest end to end, but the honest version is
  larger than a gate: multi-file ingest and per-file item mapping are real work,
  and this ADR sequences them behind the gate rather than blocking the fix on
  them. The interim leaves a pack for the right season under-served rather than
  wrongly served — a smaller, visible gap replacing a silent, wrong acquisition.
- Preferring an episode release over a pack means a want that could be satisfied
  by an already-held pack should be satisfied by reconciliation, not re-acquired —
  §3.2's blob dedup is what keeps the preference from turning into re-downloads.

## Alternatives rejected

- **A season/episode accept rule in `policy.Profile`.** Reuses §63's machinery
  with no new gate, but conflates identity with quality: "wrong episode" and
  "wrong resolution" become the same kind of rejection, and an operator loses the
  ability to tell a season-blind match from an unmet quality bar. Season and
  episode are properties of *which content this is*, not *how good it is*.
- **Parse season/episode inside `Evaluate`.** Tempting, because the title is right
  there — but `candidate.go` deliberately never parses the title (§63's
  attributes come from the provider, ADR-0091), and adding a second, invisible
  extraction at scoring time is exactly what that boundary refuses. The parse
  belongs in the gate, once, marked derived.
- **Reject season packs entirely for episode wants.** Removes the wrong-season
  selection and the multi-file ingest problem in one stroke, but season packs are
  often the only way an older season is available at all, so this trades a wrong
  acquisition for no acquisition. ADR-0091 already flagged the season-pack case as
  the thing not to walk into.
- **Trust the indexer's `tvsearch` season/ep filter as the boundary.** Structured
  search narrows results and §1 uses it, but an indexer's free-text or category
  match is not something heyarr controls, and a private tracker's aggregate search
  returns adjacent seasons routinely. The gate in §2 must stand whether or not the
  indexer filtered, for the same reason ingest hashes bytes itself rather than
  trusting a claimed hash (invariant 1).
- **Satisfy every episode want the instant a pack is grabbed, by assertion.**
  Would make §3.2 trivial, but it is the same collapse of AVAILABLE and
  CONTENT_SATISFIED that ADR-0027 and `ingestacquisition.go:166` exist to prevent:
  bytes arriving is not the profile accepting them. Per-item satisfaction stays a
  reconciliation answer over the assets the ingest actually produced.
