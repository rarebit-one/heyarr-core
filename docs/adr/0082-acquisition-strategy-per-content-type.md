# 0082. Acquisition strategy is chosen per content type

**Status:** Accepted
**Date:** 2026-09-08
**Milestone:** M12 — Followed Sources / The Archive (Phase 4, RSS / web archiving)

## Context

A followed RSS feed's articles capture, hash and ingest correctly (ADR-0063,
ADR-0080). And then they never count as archived. The reason, traced end to
end in a live deployment:

- Every followed source in the deployment had been seeded with the **`everyday`**
  quality profile. `everyday` is a *video* profile: its accept gate is
  `resolution.gte 720`.
- A captured HTML article has no resolution. `resolution.gte` therefore
  evaluates to `undetermined` — "the provider could not determine resolution,
  so this gate cannot be shown to hold" — and an accept gate that cannot be
  shown to hold does not admit the asset. `EvaluateContent` returns
  `not_satisfied` with a blob sitting right there on disk.
- `FollowStats` counts `content = 'satisfied'`, so `items_archived` stays 0
  while the bytes are present, identified (`document/title-only`) and blob-stored.

That is one symptom of a deeper shape problem. A *second* fell out of the same
trace: those same document wants were also being handed to the **search beat**,
which asked the `internet-archive` indexer for a "release" to download —
repeatedly, rate-limited, finding nothing — for content whose only correct
acquisition is *capture the page the feed pointed at*. The want had already
been satisfied-in-bytes by a direct capture; searching an indexer for it was
never going to do anything but burn a tracker's patience.

Both are the same root cause: **acquisition was one pipeline with one
video-shaped policy, applied to every content type.** The system already knows
five content types (ADR-0080) and already routes the *per-item* acquisition off
the item's shape — an item with an enclosure is grabbed directly, one without
is searched (`worker.startAcquisition`). What it lacked was any statement that a
content *type* implies a strategy: which default profile judges it, and whether
it is found by searching indexers at all.

There is no per-source way to fix this after the fact, either. The only doors
onto a subscription are follow and unfollow (ADR-0057), so "this feed is on the
wrong profile" forces an unfollow-and-refollow — which, because the web-capture
transfer ids are content-addressed and the completed transfers survive the
unfollow, strands the refollowed wants at `QUEUED` behind their own already-done
grabs. Changing a subscription's strategy needed a door of its own.

## Decision

### 1. A strategy is a function of content type

A new leaf, `internal/domain/strategy`, maps a content type to the policy that
governs how its wants are acquired:

```
type Strategy struct {
    ContentType    string
    Route          acquisition.Route // Search or Direct
    DefaultProfile string            // seeded profile name, or "" if none fits yet
}

func For(contentType string) Strategy
```

- **`RouteSearch`** — the bytes are found by asking indexers and then
  downloaded. Movies, series and video channels.
- **`RouteDirect`** — the feed already names where the bytes are; they are taken
  from there (a web capture, a podcast enclosure) and an indexer is never
  asked. Documents and podcasts.

`Route` is a value in `acquisition` (a leaf everything already imports), so
`strategy` can import `acquisition` without a cycle and the schedule policy can
take a `Route` without importing `strategy`.

### 2. The search schedule refuses a Direct-route want

`acquisition.ScheduleFor` is documented as *the only place* a want's search
cadence is decided. It gains the route:

```
func ScheduleFor(s State, monitored bool, route Route) (Schedule, bool)
```

and returns `(_, false)` for `RouteDirect` before any other test. A document or
a podcast is now never enqueued for an indexer search — the fruitless
`internet-archive` traffic stops at the source, in the one place the decision
lives, rather than being filtered downstream. `DueSearches` joins the want's
work to read its content type and passes `strategy.For(ct).Route` in.

### 3. A follow or want with no profile named inherits its type's default

`resolveProfile` previously refused an empty profile: *"this should exist" with
no statement of what would count as existing cannot be evaluated*. That refusal
is right when the type has no default, and wrong when it has one. It now takes
the content type and, given neither an id nor a name, falls back to
`strategy.For(ct).DefaultProfile` — **and a default exists only for direct-route
content**:

- a `document` or `podcast` follow inherits **`published`** (below) instead of a
  video profile it can never pass;
- a `movie`, `series`, `music` or `book` want has NO default and still requires
  an explicit profile — the §56 refusal is preserved exactly where it remains
  the honest answer. A video's quality bar is a real choice (living-room vs
  everyday vs archival) and "which did you mean" cannot be guessed; a directly-
  taken document has no such choice, which is why it is the one case that
  defaults.

### 4. A `published` profile judges directly-taken content

`policy.Defaults()` seeds one more profile, converged by name at controller
start like the others:

```
published — accept: source NOT IN (cam, telesync, workprint)
            terminal: size_bytes >= 1
```

It carries no video gate, so a captured article or a podcast episode is
accepted on the evidence it actually has, and it is *terminal on any bytes*: a
copy taken from the source that publishes it is definitive, so the upgrade loop
ends immediately rather than re-examining it forever. This is the satisfaction
*and* the upgrade half of the document/podcast strategy, expressed as a profile.

### 5. A subscription's strategy can be changed in place

`PATCH /api/v1/followed-sources/{id}` (and MCP `set_source_profile`) changes a
followed source's strategy without unfollowing it — its quality profile, its
backfill, or both. `catalog.RepointFollowedSource` updates the source in one
transaction; a profile change also moves every item-scoped want the source
projects and re-reconciles them, so an already-held asset is re-judged against
the new profile at once. A backfill change moves no want — what it changes is
which items the *next* poll projects (`shouldProject`) — so the door queues that
poll instead, the same immediacy a fresh follow gets. That second half matters
in practice: a `from_now` follow's back-catalogue has wants that exist but that
no driver ever touches (the poll skips pre-follow items before acquisition, and
the search beat now correctly ignores direct-route wants), and moving the
source to `full` is how an operator asks for it. This is the door ADR-0057 did
not have — strategy is a property of the subscription an operator can correct,
not only a thing chosen once at follow time.

## Consequences

- The 66 document feeds in the live deployment are repointed with the new PATCH
  door onto `published`; their already-captured assets re-judge to `satisfied`
  and appear as archived, and no indexer is asked about them again.
- **Music and book have a strategy but no default profile.** Their satisfaction
  is bitrate, format, edition — attributes the policy vocabulary
  (`resolution, source, video_codec, audio_codec, audio_channels, size_bytes`)
  cannot yet express, and inventing an accept-anything profile for them would
  claim a judgement the system cannot make. So `For("music")` and `For("book")`
  are `RouteSearch` with an empty default, which changes nothing for them today:
  a follow still names a profile. Giving them real profiles is a follow-up that
  starts by adding the attributes and the probes that populate them.
- `RouteDirect` is defined as "never search an indexer", which means a podcast
  whose enclosure 404s is not rescued by an indexer that might have had it. That
  is deliberate: a direct feed's enclosure is the source of record, and falling
  back to an indexer would make a podcast episode mean a different set of bytes
  than the one the feed published.
