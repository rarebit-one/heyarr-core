# 0117. OPDS offers each book asset

**Status:** Proposed
**Date:** 2026-10-19
**Milestone:** M11 — Compatibility APIs

## Context

One book Edition can hold several primary files of the same format, such as
separate comic volumes. OPDS exposed one acquisition link per Edition, so a
reader could download only the first file. Splitting an existing Edition would
change its persisted identity and any links clients already hold.

## Decision

The OPDS publication feed offers one acquisition link per primary Asset with a
blob. The first Asset of an Edition keeps the existing Edition-based download
URL. Additional Assets get Asset-based URLs and a filename link title to
distinguish them in readers. The download route accepts either kind of ID and
continues to serve only book and document content.

## Consequences

- Several files in one Edition are individually downloadable without changing
  catalog identity or breaking previously issued links.
- An Edition-based URL still names the first primary Asset by Asset ID. A
  catalog change that introduces an earlier ID could change that URL's target.
- Readers that display only one acquisition link per MIME type may still hide
  volumes. Revisit the feed shape when such a client becomes important.
