# 0081. A folder that names a season is a season of its series

**Status:** Accepted
**Date:** 2026-09-08
**Builds on:** ADR-0006 (identification is pure over a library-relative path), the M1 path heuristics (spec §66 "identify")

## Context

The path heuristics read a series tree the way a curated library lays one out:
`Show (2018)/Season 04/Show - S04E04.mkv`. The innermost directory that is not
a season directory names the series; the season directory, or the filename's
`S04E04`, names the season and the episode.

A download client lays a season out differently. It leaves one folder per
release, and the release names the series and the season in one breath, then
describes itself: `Show Season 4 Mp4 1080p/Show S04E04.mp4`,
`Show.S04.Complete.1080p.WEB/…`, or for a single episode
`Show.S04E04.1080p.WEB-DL/Show.S04E04.1080p.WEB-DL.mkv`. Scanned as a library,
every such folder is "the innermost directory that is not a season directory",
so its whole name became the series title. Measured on a node whose download
directory is also a library: three series called "Show", "Show Season 4 Mp4"
and "Show Season 5 Mp4", one per folder shape, and a person browsing sees three
of the same show (#470). Wants, satisfaction and the continue rail see three
works too — a client can fold the duplicates behind the canonical one, and
heyarr-desktop does, but that papers over the catalogue rather than fixing it.

The files' own `S04E04` markers already say what these folders are. The rule
that read them was ignoring the evidence it had.

Options considered:

- **Leave the download directory out of the libraries.** It is where the bytes
  are until an acquisition is ingested, and a scan of it is how a want learns
  it is satisfied. Rejected: the layout is legitimate, and the scanner is the
  thing that is wrong.
- **Fold duplicates after the fact, in the catalogue or the client.** A title
  heuristic over works that already exist, with all the state that has
  accreted on each. Rejected: the work key is the convergence mechanism
  (doc.go, "Convergent"); a second one beside it is a second place to be wrong.
- **Read the folder for what it names.** A name of the shape
  `<title> <season word> <n> <noise>` — or `<title> S04E04 <noise>` — names a
  series and a season, and the title is the part before the season word.
  Chosen.

## Decision

**`seriesTitleSource` splits a directory, or a filename stem, that carries a
series title and a season together: the part before the season word names
the series, the number names the season, and everything after the number is
release noise.** The season words are the ones a season directory already
accepts (`Season`, `Series`, `Staffel`, `Saison`, `Stagione`, `Temporada`,
`Seizoen`, `S`), the number may not be followed by a letter (so `S04E04` is
left to the episode rule, which then supplies both season and episode), and a
year before the season word is read as the series year as it is anywhere
else.

The split applies wherever the rules look for the series name: an episode's
directory, a season directory's parent, a companion file's promoted stem. So
`Show Season 4 Mp4 1080p/poster.jpg` is season artwork for `Show`, not a poster
for a series called "Show Season 4 Mp4".

In the show fallback — the rule for a companion file or a bare video that names
only the show — the stem now wins and the directories supply only the season.
A promoted companion file's stem *is* its show folder, so the directories above
it can only be grouping folders (`TV/`), and reading the innermost of those as
the title was wrong before this decision too: `TV/Show (2018)/poster.jpg` was a
series called "TV". The episode rules are unchanged: there a directory names
the show and the filename at best repeats it.

Nothing changes for a curated tree. The rule only fires on a name that carries
a season marker after a non-empty title; `Season 04` alone is still a season
directory, and a title that merely contains a season word without a number
(`Season of the Witch`) is still a title.

## Consequences

- A download folder and the library folder for the same season converge on
  one work key — `series:show name:2018` when the folder carries the year,
  `series:show name` when it does not — with one edition per season. A later
  move or hardlink into the library is the same work, not a duplicate.
- A folder that names the series without a year, beside a library tree that
  carries one, still yields two keys (`series:show name` and
  `series:show name:2018`). That is the general year-unknown case the path
  heuristics have always had, and Milestone 3's identifier is what resolves
  it; this decision does not try to guess a year from nothing.
- The corpus gains the download-folder shapes, with hand-written expectations
  for the load-bearing fields and an equivalence class that pins the library
  tree and every folder shape to one key.
- Series that already exist under a folder-shaped name are not renamed by this
  change: a rescan get-or-creates by work key, so their assets re-resolve onto
  the right work and the folder-named works are left empty, to be pruned as
  any empty work is.
- A library declared with a content type outside the vocabulary (`show` rather
  than `series`) is unaffected and still misreads its artwork as a movie;
  #227 validates the type on creation, and an existing library has to
  be re-declared. This decision does not add aliases to the identifier, on
  purpose: a declaration nobody checked is a different bug from a folder
  nobody read.
