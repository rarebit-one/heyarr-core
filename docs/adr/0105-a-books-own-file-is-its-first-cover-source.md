# 0105. A book's own file is its first cover source

**Status:** Accepted (2026-10-09)
**Date:** 2026-10-09

## Context

ADR-0087 gave held books a cover by asking Open Library, and on a real library
that leaves half the shelf bare. On the reference library, roughly half of
about three thousand books had no cover after enrichment had run over all of
them. The bare half was mostly not trade books. It was digitised journals,
manuals, pamphlets and conference papers, filed under a shelf name rather than
an author. Open Library has no cover for those and never will. About a third of
them had matched an Open Library work that has no cover image. Almost all were
PDFs, plus a hundred or so EPUBs.

The covers exist. An EPUB names its cover image in its own OPF manifest, and a
PDF's first page is, for a book, its cover or title page. Heyarr already holds
both files; nothing ever looks inside them.

§69 says Heyarr stores and serves publications and clients render them, and
`internal/domain/publication` enforces that. That line is about **reading**:
Heyarr does not lay out page 47 for a reader. A cover is something else. It is
the Work's **metadata**, the same `role='artwork'` asset a shipped `cover.jpg`
or an Open Library fetch becomes, and §69 lists metadata as Heyarr's to manage.

## Decision

**A held book's own file is looked inside once for its cover, and that cover
outranks one fetched from an enrich source.**

- **An `extract_cover` job per book file**, the shape of ADR-0084's
  `extract_subtitles`. It turns what Heyarr holds into an artwork asset that
  every consumer already serves.
  - For an EPUB, it copies out the image the container declares, in this
    order: the EPUB 3 `cover-image` property, the EPUB 2 `<meta name="cover">`,
    a guide `cover` reference, then a manifest image named `cover…`. These are
    plain bytes; nothing is decoded.
  - For a PDF, it renders page 1 with poppler's **`pdftoppm`**, an optional
    external specialist (§83), resolved and advertised exactly like ffprobe and
    ffmpeg (ADR-0023). No PDF or image library is linked in. A node without the
    tool extracts every EPUB cover while its PDF jobs wait, visibly. The
    capability lives on the job, not the handler.
- **A cover beat**, not an ingest hook. It finds held book files that have no
  shipped or extracted cover and have not been looked inside, and it enqueues a
  bounded batch each minute, EPUBs first. One mechanism therefore covers both
  the existing backlog and every future ingest, and there is no backfill
  command to remember.
- **Looked inside once, per blob.** `cover_extractions` records the outcome for
  the bytes, `extracted` or `none`. The job queue's dedupe key covers only live
  jobs, and finished jobs are pruned, so without this table a PDF with no
  renderable page would be reopened forever. A file that cannot be read or
  rendered is recorded as `none` and the job succeeds. Only an environmental
  failure (missing bytes, the store, the database) is retried.
- **The file's cover wins.** The asset is marked
  `identification_source='extracted'`, and `artworkRank` orders `fetched`
  last within its tier. A book that already has an Open Library cover is still
  looked inside once, and its own cover replaces the fetched one where it
  exists. The fetched asset is kept, so removing the extracted one restores it.

## Consequences

- The scanned-archive half of a library gets covers with no network and no
  provider match. Trade EPUBs show the publisher's exact cover instead of a
  search hit's.
- A PDF whose first page is a blank sheet or a library stamp gets that page as
  its cover. This is accepted. The page is still the file's own, and the
  alternative was a placeholder.
- Deploying it needs `poppler-utils` on the node for the PDF half. Without it,
  the change still delivers the EPUB half.
- No retry exists for a file recorded as `none`. Deleting its
  `cover_extractions` row makes it due again on the next pass.
- Feeds and articles (`document`) are out of scope. Their image is the source
  page's `og:image` or the feed's image, and capturing that is a separate
  change.

## What would make us revisit

- A cover is wanted for CBZ/CBR. A CBZ's first page image is a ZIP member and
  fits the EPUB path. CBR needs a RAR reader and is a separate decision.
- A better PDF cover than page 1, such as an embedded thumbnail or a
  heuristic that skips blank leading pages.
- An operator wants an explicit "re-extract" lever instead of deleting rows.
