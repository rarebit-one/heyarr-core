# 0116. Subsonic projects music assets as tracks

**Status:** Proposed
**Date:** 2026-10-19
**Milestone:** M11 — Compatibility APIs

## Context

A music Work is an album, and an Edition is a version of it (§11). The scanner
can put several song files in one Edition: all FLAC files in an album share its
format key. Each Asset carries its own disc, track and title attributes. The
Subsonic adapter instead projected one song per Edition, so a seven-file album
could expose only one playable song. Splitting existing Editions into tracks
would change persisted Edition ids, including ids already given to clients or
named by desired items.

## Decision

Project each primary music Asset with a blob as a candidate Subsonic song.
Group candidates by disc, track number and title, choosing a lossless Asset
when the same song has several formats. Emit the chosen Asset id as the song's
opaque `tr:` id. Read per-file track attributes from the Asset, falling back
to Edition attributes and then the filename for older catalog entries.

Keep `tr:` ids that name an Edition working for stream and download by serving
that Edition's first primary Asset. No Edition keys or database rows are
migrated. The adapter continues to serve only music Assets.

## Consequences

- Multi-file albums become fully browsable without rewriting canonical
  Edition or Asset identity.
- A new Subsonic song id follows its Asset across rescans of the same path.
  An old Edition-based id remains a playable alias to its former first file.
- Track grouping is only as precise as the recorded disc, number and title.
  An unnumbered file falls back to its filename; two truly different files
  with indistinguishable metadata may still be shown as one song. Revisit this
  when tag-based identification supplies stronger track identity.
