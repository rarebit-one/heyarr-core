# 0080. `document` is a first-class content type, not a reuse of `book`

**Status:** Accepted
**Date:** 2026-09-08
**Milestone:** M12 — Followed Sources / The Archive (Phase 4, RSS / web archiving)

## Context

ADR-0063 decided that a followed RSS/Atom article is archived as a
self-contained single-file HTML, attached to its own Work by the item-scoped
want, and that the Work's content type is `document` (`§12`). It noted that
`works.content_type` is free text the follow path sets, so storing `document`
needed no migration — and it did not.

But `document` was only ever WRITTEN, never REGISTERED. The identification
registry (`internal/domain/identification`) knew four content types — movie,
series, music, book — and everything that consults the registry rejected the
fifth:

- `POST /api/v1/libraries` validates the requested `content_type` against
  `identification.IsContentType` (the guard #227 added after a library declared
  as `show` grew phantom movie Works). `document` is not in that set, so a
  `document` library could not be created.
- A completed acquisition is routed to a library root by
  `catalog.RootForContentType(ctx, work.content_type)`. With no `document`
  library, that raised `ErrNoRootForContent` — *"nothing is configured to hold
  \"document\" — create a library for it"* — on every article ingest.

So following a feed, capturing its articles, and hashing the bytes all worked,
and then the pipeline had nowhere to put them. The follow path names a content
type the rest of the system will not let an operator configure for.

## Decision

### 1. `document` is registered as its own content type

`document` joins movie, series, music and book as a built-in in
`identification` — a constant, a member of `ContentTypes()`, and a set of rules
in `Default()`. Registration is what makes `IsContentType("document")` true,
which is the single fact the library-create guard and the works-patch guard
both already consult; neither needed editing. `RootForContentType` is content-
type-agnostic SQL, so once a `document` library exists an article's blob routes
to it with no change to the catalog.

### 2. A document is NOT a book

The tempting shortcut was to type captured articles as `book` and avoid a new
vocabulary word. Rejected:

- **They are different things a person browses separately.** A book is an epub
  or a comic a reader keeps; an article is a page from a followed feed. Merging
  them means an operator cannot make a library of one without the other, and a
  `GET /works?content_type=…` browse cannot separate them. `§12` names
  `Document` distinctly for this reason.
- **Their editions differ.** A book edition is a format a reader chose (epub,
  audiobook, comic); a document has one shape, the capture. Typing an article
  `book` would put an `.html` "format" beside `epub` and `mobi` on the same
  Work, which is not what either is.
- **The content type is load-bearing for identification.** `Identify` biases
  rule selection by the library's declared type. A `document` library declared
  as `book` would run the book rules over an `.html` whose name is a transfer
  digest — the exact silent-misidentification shape #227 exists to stop.

Where a document IS book-like — a titled Work whose bytes a reader downloads,
with no audio/video stream — it reuses the book plumbing rather than growing a
parallel one: it surfaces on the **same OPDS acquisition feed** (whose
navigation entry already reads "every book, comic and document"), and its bytes
download through the same `/opds/download/{edition}` path.

### 3. The document identifier is deliberately minimal

The path that matters for a captured article never reaches the identifier: the
feed named the publication and the article, the item-scoped want carries that
identity, and ingest's `WorkOverride` (`SourceDesiredItem`) attaches the blob to
that Work without parsing the filename (ADR-0063 §3). Parsing a transfer-digest
filename would be worse than useless.

The document rules therefore exist only for the OTHER path — a `document`
library scanned from disk — and are shallow on purpose: a title, and a publisher
when a directory offers one. Their real job is to CLAIM the document extensions
(`.html`, `.htm`, …) so a scanned page becomes a document Work rather than
falling through to the movie-first fallback (#227), not to extract rich
metadata a bare page does not carry.

### 4. Where `document` was deliberately NOT added

The content-type set is enumerated in a few switches that a new type does not
automatically belong in:

- **Torznab search categories (`internal/indexers`).** A document is captured
  from a followed feed, never searched from an indexer; there is no
  `documentsearch` Torznab function. An unknown type already falls back to the
  general search, so nothing breaks — but wiring one in would advertise a search
  path documents do not have.
- **DLNA UPnP classes and folders (`internal/api/dlna`).** DLNA is for AV media
  a control point renders; a document is neither audio nor video. `classFor`
  already maps an unknown type to the generic item class and `folderTitle`
  title-cases it, so a stray document degrades honestly without being claimed as
  media it is not.

Documents surface for browsing through `GET /works?content_type=document`
(ADR-0075, a generic projection needing no per-type code) and OPDS, which is
where book-shaped content already lives.

## Consequences

- A `document` library can be created, a captured article ingests into it, and
  both `GET /works?content_type=document` and the OPDS feed list it — the
  follow → capture → ingest → browse path is whole end to end.
- Captured `.html`/`.htm` bytes get a `text/html` media type at ingest, so OPDS
  advertises the download with a type a reader can dispatch on rather than an
  empty one.
- `ContentTypes()` grew a fifth member; every guard, error message and browse
  filter that reads the registry picked `document` up for free, which is the
  whole point of routing them through one vocabulary.

## What would make us revisit

- **Readability / rich document metadata.** If a future capture extracts author,
  published date or section from the article body, the minimal identifier and
  the `document` Work attributes grow to hold them — a refinement on this shape,
  not a change to it.
- **A non-HTML document shape** (a stored PDF article, or WARC per ADR-0063's
  deferral) would extend `docExts` and the MIME map, and might earn its own
  edition formats — still one content type, more editions.
- **A dedicated reading surface.** Documents ride the OPDS book shelf today
  because they are book-like enough. If document consumption diverges from book
  consumption (in-place article reading, feed-grouped browsing), that is when a
  document-specific projection earns its place over the shared one.
