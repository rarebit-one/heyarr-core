# 0085. A wanted subtitle is an aspect-scoped want, not a fifth satisfaction axis

**Status:** Accepted (2026-09-09)
**Date:** 2026-09-09
**Milestone:** M12 — Followed Sources / The Archive (Phase 5, subtitle provision)

## Context

ADR-0084 closed two of the three ways a title comes to have captions. A subtitle
the release **shipped** (a `Movie.en.srt` beside the video) is already an asset;
a subtitle **embedded** in the container (`mov_text`, SubRip/ASS) is now lifted
into one by the `extract_subtitles` job. Both paths turn bytes that were *already
present* into a `role='subtitle'` asset the caption resolver and the DLNA
`CaptionInfo.sec` header (ADR-0040) serve with no consumer change.

ADR-0084 named the remaining gap and put it out of scope: *"A subtitle that
exists nowhere — neither sidecar nor embedded — is still missing. Fetching one
from a provider network is a separate concern."* This is that concern. The
subtitle is fetched from the OpenSubtitles-class network, and — per the standing
constraint on this feature — **built into heyarr as a provider adapter, not run
as a Bazarr service beside it** (§61's "no separate applications", the same
argument that put following inside heyarr in ADR-0057).

The adapter itself (capability, kind, credential, HTTP client, fixtures) is
ordinary work modelled on the TMDB provider; it needs no ADR. **One thing here
is not ordinary, and it is the whole of this decision: how the system expresses
"this episode should have an English subtitle, and holds none."** That is a
question about desired state, and desired state already has a precise shape
(ADR-0027, ADR-0056). The tempting answer is the wrong one, and naming why is
the point of writing this down before any code.

### The tempting answer, and why it is wrong

ADR-0027 stores an acquisition as four independent facts — `phase`, `managed`,
`content`, `placement` — and its "what would make us revisit" section names the
trigger for exactly this moment: *"a third satisfaction axis — durability or
freshness, say."* Read narrowly, "do we hold a subtitle?" looks like that third
axis: add a `subtitle` `Satisfaction` to `acquisition.State` beside `content`
and `placement`, and every video want now reports whether it is captioned.

That re-collapses precisely what ADR-0027 un-collapsed.

- **`content` and `placement` are two questions about the *same blob*** — does
  *this* blob pass the profile, is *this* blob on every peer. A subtitle is a
  **different asset entirely**, with a different provenance (a subtitle provider,
  not the torznab+torrent lane), acquired at a different time (only once the
  video is held and can be hashed for matching), by a different subsystem. ADR-0027
  exists to keep "obtaining the bytes" and "replicating the bytes" from being one
  ordinal because they are *different work that regresses independently*. Obtaining
  the video and obtaining its subtitle are different work by that same test.
  Folding "has subtitle?" into the video want's state is the collapse ADR-0027
  forbids, wearing a new axis's clothes.
- **It is `not_applicable` almost everywhere.** Music, book, document and podcast
  wants have no subtitle question; nor does the video want's own primary nature.
  ADR-0027 already calls each `not_applicable` special-case a smell — "the fifth
  place they need a special case" is written there as a complaint, not a licence
  to add a sixth.

The subtitle question is real and new. It is just not an axis of the video's
want. It is a **different want**.

## Decision

**A wanted subtitle is its own item- or edition-scoped `DesiredItem`,
distinguished by an `Aspect`, whose satisfaction is ordinary content
satisfaction over the assets of that aspect. It reuses the entire acquisition
spine; the one genuinely new domain piece is that content satisfaction — and
want identity — become parameterised by the aspect of the target the want is
about.**

### 1. `desired.Item` gains an `Aspect`

`desired.Item` (`internal/domain/desired/desired.go`) carries a new field:

```
Aspect  Aspect  // AspectPrimary (default) | AspectSubtitle{Lang}
```

`AspectPrimary` is the content itself — the video, the audio, the document — and
is what every want made before this change is. `AspectSubtitle` names a
companion facet of the same target, carrying an ISO-639-1 language (`en`
first). The aspect is *not* a new scope and *not* a new Item: a subtitle is not
a thing a source emitted (ADR-0056's test for Item-hood), it is a facet of the
episode or movie the source already emitted. So it attaches to the **existing**
target — the episode's `Item`, or a film's `Edition` where there is no Item —
never a fabricated one.

`SameWant` (`desired.go`) folds `Aspect` into identity alongside `Target()` and
`QualityProfileID`, exactly as ADR-0056 folded scope in: "the episode" and "the
episode's English subtitle" become two distinct wants on one target, the same
way two quality profiles of one episode are two wants. `Validate()` permits
`AspectSubtitle` only at a scope that names concrete video bytes (item, or
edition) — an aspect is meaningless at the source-Work scope whose satisfaction
is the completeness *fold*.

### 2. Satisfaction is `EvaluateContent` unchanged, over the aspect-selected assets

`EvaluateContent(assets []AssetView, profile) ContentVerdict` already takes a
**caller-supplied** asset set and returns the best that passes. Content
satisfaction for an `AspectPrimary` want passes the target's content assets, as
today. For an `AspectSubtitle{en}` want the caller passes the target's
`role='subtitle'`, language-`en` assets. **The evaluator does not change** —
this is ADR-0056's "content satisfaction for an item want is `EvaluateContent`
unchanged" carried one step further: the aspect selects the candidate set, the
evaluator judges it. The edition-level `EXISTS ... role='subtitle'` test the
ADR-0084 backfill already uses (`internal/api/resources/subtitles.go`) is the
same predicate this reads as a satisfaction verdict rather than a job filter.

`EvaluateCompleteness` (the ADR-0056 fold) then extends for free: "every episode
of this series holds an English subtitle" is the fold over the per-episode
subtitle wants, identical in shape to "every episode is archived."

### 3. Route is aspect-first; a subtitle is accepted by provenance

A subtitle want is **always `RouteDirect`, whatever the video's content type.**
`strategy.For(contentType)` (ADR-0082) says a series is `RouteSearch` — correct
for the episode, wrong for its subtitle, which is never found by asking a torznab
indexer. So the route decision consults the aspect first: `AspectSubtitle`
resolves to `(RouteDirect, subtitle-profile)`; `AspectPrimary` falls through to
`strategy.For(contentType)` unchanged. `ScheduleFor` already takes a `Route`
(ADR-0082 §2) and so needs no change — it is handed the aspect-resolved route
and, being `RouteDirect`, is never enqueued for an indexer search.

A fetched subtitle carries none of the video vocabulary a quality profile ranks
(`resolution, source, codec, …`), so — exactly as a podcast enclosure (ADR-0060
§3) and a captured document (ADR-0082 §4) — it is accepted by **provenance**,
not evaluated against a video profile. `policy.Defaults()` seeds one more
profile, converged by name at controller start like `published`:

```
subtitle — accept: role == subtitle
           terminal: size_bytes >= 1
```

Terminal on any bytes: a subtitle the provider chose is definitive, so the
upgrade loop ends rather than re-examining it forever. Any *ranking* among
candidate subtitles (hearing-impaired vs not, download count, uploader trust)
is the **adapter's** business, not the profile's — the direct-route stance that
the source, not the profile, chooses which bytes (ADR-0060 §3).

### 4. The provider searches inside the adapter; the pipeline sees a direct release

OpenSubtitles must be *queried* — by the video's moviehash, or by external id +
season/episode — and returns ranked candidates. That query and that ranking live
**entirely inside the adapter**. The adapter hands the pipeline a single chosen
download URL, and `catalog.RecordDirectRelease` (ADR-0060 §2) stores it as the
want's one pre-selected release, walking the genuine `search → candidates_found
→ select` edges. From the acquisition pipeline's view this is the
podcast-enclosure shape: a direct release the indexer machinery never touches.
The query *is* the search, the same way a followed source's poll *is* its search
(ADR-0060) — the adapter is the identity authority for its source. `KindHTTP`
(`internal/downloads`) grabs the URL; `ingest.Pipeline.Ingest` attaches it to
the known video via a `Request.Work` `WorkOverride` and a `RelPath` whose
extension and language make `identification/roles.go` assign `RoleSubtitle`
(ADR-0014/0084). The asset lands on the edition, and the subtitle want's
aspect-scoped `EvaluateContent` now reads satisfied — with no new consumer code,
the caption resolver and the DLNA header serving it like any sidecar.

### 5. It is a want, not a job — and it composes with ADR-0084

ADR-0084 made extraction a **job** because the bytes were already in the file:
no desired-state question, pure processing. A **fetched** subtitle is genuinely
absent and must be searched for over an external, quota-limited,
capability-routed provider (ADR-0025) that may return nothing; it needs candidate
selection, a retry cadence, and a resting "we looked and found none" state. That
is a desired-state question walking the four-fact machine — a **want** (ADR-0056/
0057), not a job.

The two compose as **extract-first, fetch-as-fallback**, and a subtitle want's
acquisition is **gated on its target's primary content being satisfied.** You
match a subtitle by the held video's hash and attach it to a held edition, and
ADR-0084's shipped/embedded extraction runs first — so a subtitle is fetched from
the provider only when the video is held *and* no subtitle of the wanted language
is present after extraction, neither shipped nor embedded. The want may *exist*
before the video does (a follow projects it for a freshly-aired episode, §6), but
it rests in `MISSING`/`AVAILABLE` until the primary is held; the direct-fetch is
never the first thing tried and never runs while the bytes might already be in the
file. This gate is the one dependency between two otherwise-independent wants, and
it is a precondition on the *fetch*, not a fact folded into the video want's state
— the ADR-0027 separation the aspect model exists to preserve.

### 6. A followed source can carry a standing subtitle aspect

Provision has two doors, both landing on the same want through the one shared
creation path (`catalog.CreateDesiredItem`, ADR-0059 §2), so a requested want and
a projected want are byte-identical:

- **Requested** — an explicit per-work/episode API+CLI request, and a
  library-scoped backfill that is the exact mirror of ADR-0084's `subtitles
  backfill`: it scans managed videos in scope and, for each held video lacking the
  wanted-language subtitle, creates a subtitle want.
- **Standing** — a `FollowedSource` (ADR-0057) carries a subtitle aspect
  (`want_subtitles: [en]`). Its projection, which today emits one primary
  `DesiredItem` per enumerated item, additionally emits an `AspectSubtitle{en}`
  want on the same target. Because acquisition is gated on primary satisfaction
  (§5), the subtitle want simply waits for the episode's video to land and then
  fetches — captions-on-arrival with no per-title request. This is ADR-0057's
  "following = enumerate + project wants" extended by one projected want per
  configured language, not a new pipeline; `set_source_profile` /
  `PATCH /followed-sources/{id}` (ADR-0082 §5) is the door that turns the aspect
  on or off for a subscription in place.

The standing door is what makes subtitle provision keep pace with a followed
series; the requested door is how a one-off library or an already-followed
back-catalogue is caught up. Both are bounded by the same quota discipline
(Consequences), and neither invents state a subtitle want does not already have.

## Consequences

- **The acquisition spine is untouched.** DesiredItem, ScopeItem, the four facts,
  `RouteDirect`/`RecordDirectRelease`, the grab/verify/ingest/replicate path, and
  the completeness fold all carry a subtitle want with no change beyond the aspect
  field and the aspect-first route lookup. A fetched subtitle replicates to both
  Full Peers by construction (M4/M5), the same free win ADR-0057 noted.
- **Rate limiting and response caching are genuinely new infrastructure.** No
  adapter in `internal/providers/**` has either; the closest prior art is the
  indexer client's capabilities cache and its *reactive* 429 backoff
  (`internal/indexers/client.go`), which is not a proactive limiter. OpenSubtitles
  enforces hard per-window quotas, so the adapter needs a client-side limiter and
  a subtitle-result cache, and the want's re-poll cadence for a
  found-nothing subtitle must respect the same budget. This is the one place the
  feature invents a pattern rather than copying one; it is flagged, fixtures-tested
  (ADR-0026 — the provider is never hit live in CI), and its own PR.
- **Provision has both a requested and a standing door (§6), and both feed the
  quota.** The per-work/episode request and the library backfill are the
  human-driven catch-up doors; a subtitle aspect on a `FollowedSource` is the
  standing door that keeps a followed series captioned on arrival. Because the
  standing door emits a subtitle want per episode automatically, the quota
  discipline below is not optional polish — a followed series' whole run projects
  subtitle wants, and the limiter plus the primary-satisfaction gate (§5) are what
  keep that from a quota stampede. The aspect being a projection property means a
  subscription's subtitle intent is corrected in place (`PATCH`/`set_source_profile`,
  ADR-0082 §5), not by unfollow-and-refollow.
- **Language generalises but English is the only default.** `AspectSubtitle`
  carries a language from the start, so a later "also want German" is another want
  on the same target, folded distinctly by `SameWant`. Nothing here special-cases
  English beyond it being the seeded default.
- **The 0084 duplicate-subtitle caveat still stands.** ADR-0084 left a known
  simplification: an edition can hold both an extracted and a sidecar subtitle for
  one language, and the resolver serves the first stem match. A fetched subtitle
  adds a third possible source of the same-language duplicate. The extract-first
  ordering keeps a fetch from *starting* when a subtitle is already held, but the
  language-level dedupe the resolver still owes is unchanged by this decision and
  remains that ADR's follow-up.

## Alternatives considered

- **A fifth `subtitle` satisfaction axis on `acquisition.State`** (the framing
  ADR-0027's "third axis" note invites). Rejected in Context: it collapses two
  independent acquisitions into one want's state — the exact defect ADR-0027
  exists to prevent — and is `not_applicable` on nearly every want, adding the
  special case ADR-0027 already treats as a smell. The aspect-scoped want keeps
  the four facts honest and confines the new concept to *which assets are this
  want's candidate set*.
- **A first-class subtitle `Item`.** Rejected: an Item is a thing a *source
  emitted* over time (ADR-0056); a subtitle is a facet of an already-emitted
  episode, resolving to one of that Item's assets. Making it an Item invents rows
  for something the feed never enumerated and detaches the subtitle from the video
  it captions.
- **A `fetch_subtitles` job, mirroring ADR-0084's `extract_subtitles`.**
  Rejected in §5: a job is right for bytes already present and wrong for bytes
  that must be found over a fallible, quota-limited network with a resting
  "found none" state — that is what the want machine is for.
- **Reuse the `published` profile rather than seed a `subtitle` one.** Tenable —
  both are provenance-accept and terminal-on-any-bytes — but `published` also
  encodes document/podcast source exclusions (cam/telesync/workprint) that mean
  nothing for a subtitle. A dedicated `subtitle` profile says what it accepts
  honestly and leaves room for a real subtitle-ranking vocabulary later without
  overloading a document profile.

## What would make us revisit

- **A ranking vocabulary for subtitles.** If the choice among candidates should
  be governed declaratively (prefer non-SDH, minimum download count) rather than
  by adapter judgement, the `subtitle` profile grows real gates and the
  provenance-accept stance narrows — the same evolution ADR-0060 anticipated for
  any direct-route source that later needs its profile to gate.
- **Bitmap subtitles.** ADR-0084 skips PGS/VobSub because they need OCR; a
  provider that offers only an image subtitle for a title raises the same OCR
  question here, and the answer (skip, or a separate OCR capability) would be its
  own decision.
