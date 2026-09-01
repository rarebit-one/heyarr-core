# 0063. A followed article is archived as a self-contained single-file HTML capture

**Status:** Accepted
**Date:** 2026-09-01
**Milestone:** M12 — Followed Sources / The Archive (Phase 4, RSS / web archiving)

## Context

Phases 2 and 3 (ADR-0060, ADR-0062) established that a followed feed item whose
bytes location the feed already names is a DIRECT release: the poll projects it
and an existing or new download client fetches it, with nothing to search. A
podcast enclosure is fetched over HTTP; a YouTube video is fetched by running
yt-dlp. Both produce an ordinary media file that the content model already holds.

Phase 4 adds generic RSS/Atom web feeds — "the archive of our lens of the
internet". An article does not fit the pattern the way audio and video did:

**An article's bytes are an HTML page, and the page is not the archive.** A raw
page references stylesheets, images, fonts and scripts on the network; stored as
fetched, it renders as a broken skeleton the day the origin changes a path or
disappears — which is exactly when an archive is supposed to still work. And an
article is a NEW shape of content: `§12` names `Document`, but nothing in the
content model produced one before this phase.

The plan gave three options: (A) store the raw page, (B) a self-contained
single-file HTML with subresources inlined, (C) a full WARC/web-archive. B was
recommended and chosen.

## Decision

### 1. The capture is a self-contained single-file HTML, produced by a download client

A new `KindWebCapture` download client fetches the article page, inlines each
stylesheet as a `<style>` and each image as a `data:` URI, drops external scripts
and any subresource it cannot fetch, and writes ONE `.html` file. The invariant is
that **no network reference survives**: a subresource that will not fetch degrades
the archive (a missing image, an un-styled block) but never leaves a live URL in
it, so the file renders identically years later against nothing.

This is a download client and not a new subsystem for the reason ADR-0062 gave
for yt-dlp: everything downstream of a completed transfer — the path map, ingest,
hashing, replication, GC — wants an ordinary file on disk, and a self-contained
`.html` IS one. So the entire CAS/replication/verify machinery carries a captured
article for free, exactly as it carries a movie. The client differs from the http
and yt-dlp clients only in that it SYNTHESISES the bytes it stores (page + inlined
subresources) rather than moving bytes that already exist as a file.

WARC (Option C) is deferred: it is a second content shape and a parser/ingest
path of its own, and Option B delivers the durable-archive property — renders
offline, forever — with the file model that already exists.

### 2. Routing reuses the transport tag, so it is independent of registration order

An article URL is `http(s)`, so if the `KindWebFeed` adapter recorded it as a
bare enclosure the plain-HTTP client would claim it first and store the raw,
dependency-laden page. The adapter instead tags it `followed.WebCaptureSourceScheme +
articleURL` (`webcapture:https://…`), the same seam ADR-0062 introduced for
yt-dlp: `KindHTTP` refuses it (its scheme is no longer `http`) and `KindWebCapture`
claims only the tagged form. Nothing at the projection site changes — the poll
still reads `EnclosureURL()`.

### 3. The archive attaches to its known Work; it is not re-identified by filename

A media file dropped into a library is identified by its name; a captured article
must not be, because its name is a transfer digest and its identity is already
known — the feed said which publication and which article. The item-scoped want
carries that identity, and ingest's `WorkOverride` path (`SourceDesiredItem`)
attaches the captured Blob to the article's own Work rather than running
filename identification over an `.html`. The Work's content type is `document`
(`§12`), stored on the work row like any other content type — `works.content_type`
is free text the follow path sets, so no schema migration is needed to hold it.

## Consequences

- Web archiving works with no change to the CAS, the grab, ingest, verify,
  replication or GC paths — a captured article is a managed Blob like any other
  and replicates to both Full Peers by construction.
- The `webfeed` provider kind needs no endpoint and no credential — the feed URL
  is the source's own `FeedRef` — and the `web-capture` client needs none either:
  it captures by fetching a page, not by reaching a configured service.
- `golang.org/x/net/html` is promoted from a transitive to a direct dependency:
  the capture rewrites the DOM (find stylesheets and images, replace them), which
  a real HTML parser does correctly and a regex does not. It is the Go team's
  quasi-stdlib parser, already in the module graph, consistent with the existing
  direct `x/crypto`/`x/sys` dependencies.

## What would make us revisit

- **Reachability through the follow API.** As of this phase the follow API's
  `inferFeed` classifies a source type from the URL alone and treats every
  non-TVDB `http(s)` URL as a podcast — so it cannot yet distinguish an article
  feed (or a YouTube channel) from a podcast, and neither Phase 3 nor Phase 4 is
  reachable end-to-end through the API without an explicit type from the caller
  and a poll that routes by source type to the matching adapter. Both mechanisms
  exist and are tested behind the FeedProvider/Downloader seam; wiring inference
  and per-type routing is the deliberate next step, not this phase's.
- Readability extraction (stripping chrome to the article body) is a refinement
  on top of the self-contained capture, not a change to it, and can be added as a
  capture option without touching the storage shape.
- A paywalled or authenticated article needing a credential would put one on the
  `webfeed`/`web-capture` kinds — deliberately not built now, the same boundary
  ADR-0060 and ADR-0062 drew.
- If the durable-archive requirement grows to "the exact bytes, headers and all"
  (legal-grade provenance), that is when WARC (Option C) earns its second content
  shape.
