# 0115. Known media types inherit searchable starting profiles

**Status:** Proposed
**Date:** 2026-10-19
**Amends:** ADR-0082, section 3 and its music/book consequence

## Context

ADR-0082 gave only direct-route documents and podcasts a default profile. A
movie, series, album or book want still had to name one. In practice, a person
saying "I want this" expects Heyarr to start searching. The CLI blocked the
request before it reached the controller, and the API returned 400. Operators
then reused video-shaped or source-gated profiles for music and books. Torznab
results often omit `source`; an `accept: source not in (...)` rule therefore
evaluates to undetermined and rejects every candidate, even when the release
title and size are available. The want remains missing with no download.

The existing vocabulary still cannot judge music bitrate or book format. The
choice is whether to keep requiring a custom profile before search can start,
or supply an explicit and modest starting standard using facts the indexer can
actually report.

## Decision

Every known content type has a seeded default profile. An explicit profile on
the want or followed source always wins. Unknown types still require one.

| Content type | Default | Acceptance and stop condition |
| --- | --- | --- |
| Movie, series, searched video | `everyday-indexed` | 720p minimum; 1080p stops upgrades. Identified cam-like sources score lower, while an unknown source does not block a release. |
| Music | `music-indexed` | A nonempty release is accepted and stops upgrades. |
| Book | `book-indexed` | A nonempty release is accepted and stops upgrades. |
| Document, podcast | `published` | The direct source remains definitive, as in ADR-0082. |

These are **starting profiles**, not claims that an MP3 is lossless or a PDF is
the desired edition. Their descriptions state what they cannot judge. A user
who cares about format, size or 4K chooses or edits a profile. Existing wants
keep their selected profiles; controller startup seeds missing names without
overwriting any profile by that name.

The API resolves the default by content type, and the CLI allows an omitted
`--quality-profile` so both entry points mean the same thing. Wanting still
queues reconciliation and search; a missing indexer result or a transfer with
no peers can still prevent acquisition.

## Revisit when

The policy vocabulary and release probes carry reliable music format,
bitrate, book format and edition attributes. At that point, replace the
size-only starting profiles with standards that can judge those properties,
without silently changing existing operator-edited profiles.
