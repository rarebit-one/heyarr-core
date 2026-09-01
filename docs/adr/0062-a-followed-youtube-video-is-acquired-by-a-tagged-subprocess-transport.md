# 0062. A followed YouTube video is acquired by a tagged subprocess transport

**Status:** Accepted
**Date:** 2026-09-01
**Milestone:** M12 — Followed Sources / The Archive (Phase 3, YouTube channel following)

## Context

Phase 2 (ADR-0060) made podcast following "nearly free": a public RSS feed both
discovers episodes and names their bytes location (`<enclosure>`), a direct
http(s) URL the existing `KindHTTP` download client fetches. Following = enumerate
+ project (as a direct release) + get out of the way.

Phase 3 adds YouTube channel following, and it reuses almost all of that: a
channel's public feed at `youtube.com/feeds/videos.xml?channel_id=…` (Atom, no API
key) enumerates recent videos, and each becomes a direct-release `DesiredItem`
exactly as a podcast episode does. One thing does not carry over, and it is the
whole of this decision.

**A podcast `<enclosure>` is the audio file. A YouTube `<link>` is a watch
*page*, not the video bytes.** Turning that page into a media file is precisely
what `yt-dlp` exists to do, so acquiring a followed video means running an
external tool — the first transport in the system that is a **subprocess** rather
than an HTTP fetch or a torrent daemon. And the watch URL is itself `http(s)`, so
if it were recorded as a bare enclosure the plain-HTTP client would claim it first
(a registry `Grab` tries download clients in order, first success wins) and fetch
the HTML page instead of the video.

## Decision

### 1. The source is transport-tagged, so routing is independent of registration order

A feed adapter that knows a source needs `yt-dlp` records its enclosure as
`followed.YtDlpSourceScheme + watchURL` (`ytdlp:https://www.youtube.com/watch?v=…`)
rather than the bare URL. This keeps the routing decision on the source's SHAPE,
the way `KindHTTP` routes on the `http(s)` scheme and a torrent client on
`magnet:`:

- `KindHTTP.Add` parses the source; its scheme is now `ytdlp`, not `http(s)`, so
  it **refuses** — the video no longer looks like its to take.
- `KindYtDlp.Add` accepts **only** a source with the tag, strips it, and hands the
  bare watch URL to the tool; it refuses everything else.

Routing on the tag rather than on the `youtube.com` host is what makes the choice
independent of the order the two clients happen to be registered in. Coupling
`KindHTTP` to a list of YouTube hosts — the obvious alternative — was rejected: it
teaches the plain-HTTP client about a source it has no other reason to know, and
it breaks the moment a followed video lives on a host that list has not seen. The
tag lives beside `AttrEnclosureURL` in `followed`, the one place the
non-search-source-to-acquisition seam is defined, so both the adapter that writes
it and the client that strips it read it from a single constant.

Nothing at the projection site changes: the poll still reads `EnclosureURL()`,
still routes a non-empty enclosure to `RecordDirectRelease` (ADR-0060). A YouTube
video is a direct release like a podcast episode; only the download client that
finally accepts it differs, and that is decided by the source shape at the grab.

### 2. yt-dlp is a system dependency on PATH, and its absence degrades gracefully

The tool is expected on `PATH`, not vendored, wrapped in a container, or run as a
sidecar (a deploy decision, plan §8). A host without it does not fail to
construct the client or to register the transfer — the transfer simply completes
with a named `Error` ("`yt-dlp` not found on PATH …") the operator can act on,
exactly as an unreachable HTTP server surfaces through a transfer's `Error` rather
than a construction failure. The client is otherwise an ordinary `Downloader`:
`Add` returns as soon as the transfer is registered and the subprocess runs on a
detached, timeout-bounded goroutine, so a completed grab does not cancel a
download in flight and a stuck process cannot pin a transfer forever.

### 3. The subprocess is exercised only against the live tool, never in CI

Per ADR-0026 an external service (or tool) is never driven in CI. The runner is
injected — a `Runner` interface — so the unit tests prove the whole `Add → register
→ complete / fail` wiring against a fake, deterministically and with no `yt-dlp`
present, the same way `KindHTTP`'s tests prove its fetch against an httptest
fixture. The real `yt-dlp` invocation is the one thing the tests do not run; it is
proven by driving it against the live tool.

## Consequences

- YouTube following works on a node with no indexer, like podcast following —
  the follow abstraction generalises to a third source type with no change to the
  content model, the grab, verify, ingest or replication paths. A downloaded
  video is a managed Blob like any other.
- The `youtube` provider kind needs no endpoint and no credential — the channel
  feed URL is the source's own `FeedRef`, and a public feed authenticates
  nothing.
- The system now has a subprocess transport, and the `Runner` seam is where any
  second one (a different site's downloader) would slot in behind the same
  refusing-`Add` contract.

## What would make us revisit

- If discovering brand-new channels not yet in the library becomes a goal, a
  metadata-search mode (resolving a channel handle to its feed) is a separate
  enhancement — this phase follows a channel whose feed URL is already known.
- A private or age-gated video needing authenticated `yt-dlp` (cookies, a
  credential) would put a credential on the `youtube`/`yt-dlp` kinds — deliberately
  not built now, the same boundary ADR-0060 drew for an authenticated podcast
  feed.
- If a captured-source phase needs the quality profile to actually gate a
  direct release, the provenance-accept stance (ADR-0060 §3) becomes per-type; the
  tag introduced here already lives on the adapter, where that choice would move.
