# 0092. Discovery is one search, and the result — not the door — carries the action

**Status:** Proposed (2026-09-10)
**Date:** 2026-09-10
**Milestone:** M12 — Followed Sources / The Archive (Phase 6)

> This ADR number is **0092**. 0091 is left for a concurrent change already in
> flight (release attributes derived from the title); the ADR sequence tolerates
> a gap (see ADR-0074), and claiming 0091 here would collide with that PR.

## Context

Finding something to watch, hear or read is spread across four MCP verbs that a
person does not think of as four things:

- `search_content` searches **only the library** — works already held, resolved
  by title so a later want can name a work by id rather than by description
  (tools.go:36).
- `discover_content` asks the **metadata provider** (TVDB today) for candidate
  series *not* in the library — the "search then follow" door (tools.go:54).
- `want_content` declares a one-off desire (tools.go:82); `follow_source`
  declares a standing subscription that archives a series or podcast forever
  (tools.go:136).

ADR-0089 already fixed the *write* half of this seam on the server: a work-scoped
`want_content` on a **series** no longer creates a bare want — the handler reads
the work's content type (`resources.WantContent` gate, desired.go:409-423;
`establishSeriesFollow`, desired.go:512; `workTypeAndTitle`, desired.go:499) and
establishes a `FollowedSource` instead. Because that inference is driven by the
*work*, not by the caller, the server now does the right thing **whichever door
the want arrives through**. Its own "Alternatives considered" named a unified
`acquire()` door as the eventual consolidation and deferred it. This ADR is that
consolidation's other half — the **read/discovery** side — and it explains why
0089's server fix did not, by itself, make the seam disappear for a user.

The seam the user actually hit is in the first-party client, and it is a real
inconsistency. Adding a show from the **Discover** tab produced a proper follow
with its episodes projected as wants; adding the *same* show from the **Search**
tab produced a single one-off want. Both screens call `want_content` — neither
reaches `follow_source` — but with a **per-screen hardcoded shape**, not one
inferred from the result:

- The Search screen's `+Want` sends `want_content{work_id}` with **no content
  type** (SearchScreen.kt, the `SearchRow.WorkRow` result row; `WantRequest`
  defaults to `MediaType.MOVIE`). This only lands as a follow when the library
  work is *already* typed `series` and the server bridge fires; the result row
  itself carries the hit's real content type but never forwards it into the
  action, and the button is framed as a one-off "want" regardless.
- The Discover screen's `Want` sends `want_content{title, content_type:"series"}`
  with `series` **hardcoded for every hit** (DiscoverScreen.kt,
  `DiscoveryResults`), so it behaves like a follow — and would mis-file a
  discovered movie or album as a series.

So the door you reach for changes the outcome, and each door has baked in an
assumption (Search: "everything is a one-off want"; Discover: "everything is a
series") that is wrong for half of what flows through it. This is the same class
of defect ADR-0089 removed on the server — an action chosen by *where the intent
entered* rather than by *what the intent is about* — surfacing one layer up, in
the product, because the discovery surface was never unified to match.

The two-tab split has a second cost independent of the action bug: a user must
know *in advance* whether a title is held (search the library) or new (discover
from a provider), and choose the right tab, to find it. That is the library's own
ledger leaking into the search box — the opposite of ADR-0075's "open on a shelf,
not a ledger".

## Decision

**Discovery is one search. A single query fans out to the library and to the
metadata providers, the results are merged into one identity-deduplicated list
with held works on top, and each result carries a single primary action that the
server infers from two facts — is it held, and its content type. The client
renders and executes that action; it never chooses between want and follow.**
`want_content` and `follow_source` become the implementation of a result's
action, not doors the user picks between.

### 1. One query, both arms, in parallel

A unified discovery surface fans a free-text query to `search_content` (the
library arm) **and** `discover_content` (the provider arm) concurrently and
returns one merged result set. The two existing verbs stay as the primitives
underneath — `search_content` is still the exact by-id resolver other tools lean
on ("resolve it with search_content first", renderers.go:102), and
`discover_content` is still the provider probe — but the *discovery* experience a
person uses is the composed surface, not a choice between the two. Neither arm
blocks the other: a node with no metadata provider still returns its library arm
(the provider arm reports "no provider configured" the way `discover_content`
already does, tools.go:63), and a slow indexer never stalls held results.

### 2. Results are de-duplicated by identity, held wins

A provider hit and a library work are the **same result** when they share an
identity — a stored external id (`tvdb`/`tmdb`, the reconciliation path of
ADR-0050, reachable via `get_external_ids`) or a resolved work id. When they
match, the two collapse into one result presented as **held**: a series you both
own and that the provider also lists appears once, as owned, not twice. Held
results rank above not-held ones; within each group the existing browse ordering
applies (title, or newest-added — ADR-0075). De-duplication is by identity, never
by fuzzy title equality, so a remake and its original stay distinct.

### 3. The primary action is inferred, never chosen

Each result carries exactly one primary action, computed by the server from
`(held?, content_type)` — the same inference principle ADR-0089 applied to the
write path, now applied to the read path:

| held? | content type | primary action |
|---|---|---|
| held | movie, episode, album, track, book, document | **Play** (resume via the ADR-0024 session; the result names the one file a tap would play, per ADR-0075) |
| held | series | **Open** — you already hold it (and, via ADR-0089, follow it); the action opens its episodes (the ADR-0075 projection), it does not re-want |
| not held | series | **Follow** — a standing subscription (ADR-0089/ADR-0057) |
| not held | movie, album, book | **Want** — a one-off desire (`want_content`) |

The action is a property of the *result*, decided by heyarr-core, and the client
executes the verb it names. A client MUST NOT hardcode want-vs-follow per screen,
and MUST NOT default a result's content type (the two present desktop bugs). This
is why 0089's server fix was necessary but not sufficient: the server already
routes a series want to a follow, but only a *client that stops choosing the verb
itself* stops producing the wrong shape before the request is even sent (a
Discover hit hardcoded to `series`, a Search hit sent with no type at all).

### 4. `want_content` and `follow_source` are the action's implementation

"Get this" resolves to whichever verb the inferred action names — `follow_source`
(or the 0089 want→follow bridge) for a not-held series, `want_content` for a
not-held movie/album/book — reusing every existing acquisition path unchanged. The
tail of each verb still governs itself: a movie want remains ADR-0077's deferred
"want-scoped discovery candidate", and a music or book want still requires an
explicit quality profile because its type has no default (ADR-0082 §3). The
unified surface does not change *what* those verbs do or acquire; it removes the
user's need to know which one to call.

### 5. Acquisition search is a different axis and stays out

`search_releases` — "ask the indexers for releases that would satisfy a want,
now" (tools.go:108) — is **not** discovery. It answers "what can I grab for a
thing I already want", per-want, over indexers (ADR-0025/0028, and now the
Prowlarr aggregate of ADR-0090); its results are releases graded against a
profile (ADR-0027), not works. A unified discovery result MAY show a held work's
**acquisition status** as a summary — e.g. "8 wanted, 0 held" from
`get_content_satisfaction` / `get_missing_content` — but it MUST NOT fold raw
indexer releases into the discovery list. Discovery answers "what is this and do
I have it"; release search answers "which bytes for this want". Merging them
would put a tracker's transient release list in the same surface as the catalog,
which is the confusion ADR-0028 kept apart by binding to the protocol, not the
product.

## Consequences

- **The door stops deciding the outcome.** A series is followed and a movie is
  wanted whether the user found it in the library or from a provider, because the
  action rides the result. The desktop's two hardcoded assumptions
  (Search → one-off, Discover → series) are deleted, not patched — the client
  reads the action instead of inventing it.
- **One search box, no tab to pre-guess.** A user types a title without first
  deciding whether it is held; held results simply sort to the top. This is the
  shelf, not the ledger (ADR-0075).
- **The provider arm is only as wide as `discover_content` is.** Today that is
  TVDB series (tools.go:58); movie/music/book discovery is still ADR-0077/0087's
  deferred provider path. The unified surface is defined now and its provider arm
  widens as `discover_content` gains those types — no rework of the merge, dedup
  or action inference when it does, because those turn on content type, which is
  already the axis.
- **Held-container vs held-leaf is an honest split in the action table** (§3): a
  held *series* opens rather than plays, because a series is not a single file.
  This keeps "Play" meaning play and avoids pretending a container is a stream.
- **A new server-side merge/dedup step exists** that neither verb had before — two
  provider calls, an identity reconciliation, a rank. It is bounded (the provider
  arm is already paged and already the slow path in `discover_content`), and it
  runs read-only, but it is real new surface with its own tests, not free.
- **A wrong provider match is as recoverable as ADR-0089's** — the follow or want
  a result triggers is reversible (unfollow / re-want), so identity dedup erring
  toward "these are different" (show twice) is safer than erring toward "same"
  (hide a distinct work), and the merge is tuned that way.

## Alternatives considered

- **Fix only the desktop: forward the hit's real content type from both screens
  and let 0089's server bridge do the rest.** This is the smallest change and it
  *would* remove the specific bug the user hit. Rejected as the whole answer
  because it leaves the two-tab split — the user still has to guess held-vs-new
  before searching — and leaves every future client free to re-invent the same
  per-screen action choice. The action belongs on the result, decided once by the
  server, not re-derived in each client. (This ADR still *requires* that desktop
  stop choosing the verb — §3 — but as a consequence of the unified surface, not
  as the fix.)
- **A single mega-verb that also returns indexer releases** ("search finds
  everything"). Rejected (§5): it merges the catalog axis with the acquisition
  axis ADR-0028 deliberately separated, and puts transient, profile-graded
  release rows beside stable works. Acquisition status as a summary is the right
  amount of that information in discovery.
- **Let the client fan out and merge (keep the two verbs, compose them in the
  UI).** Rejected: it puts identity reconciliation and action inference in every
  client, which is exactly how the two clients drifted apart in the first place.
  The server owns identity (ADR-0050) and content type, so the server owns the
  merge and the action.
- **Drop `search_content`/`discover_content` in favour of the unified verb.**
  Rejected: `search_content` is the exact by-id resolver other tools depend on
  (renderers.go:102, `get_external_ids` inputs), and `discover_content` is the
  narrow provider probe; both are useful primitives. The unified surface composes
  them, it does not replace them.
- **Rank by relevance across held and not-held together (no held-on-top rule).**
  Rejected: "do I already have this" is the first question a discovery result must
  answer, and a strong title match from a provider outranking a work the user owns
  would bury the answer. Held-on-top is the product stance, not a scoring tie-break.

## What would make us revisit

- **`discover_content` gaining music and book providers** (ADR-0077/0087) — the
  merge and action table already turn on content type, so this widens the provider
  arm without reshaping the surface, but the album/book **Play** and **Want** tails
  (profiles, acquisition) land with those ADRs, not this one.
- **A confidence gate on identity dedup**, mirroring ADR-0088, if top-match
  reconciliation collapses distinct works often enough to want a "these look like
  the same title — are they?" surface rather than always erring toward two rows.
- **A movie becoming followable** (ADR-0077's want-scoped discovery candidate) —
  the action table's "not held movie → Want" row is where that lands, and it may
  grow a standing variant then.
- **The unified `acquire()` door ADR-0089 deferred** — this ADR unifies the read
  side; once the movie shape lands, folding the write verbs behind one "get this"
  server action (rather than two the result chooses between) is the matching
  write-side consolidation.
