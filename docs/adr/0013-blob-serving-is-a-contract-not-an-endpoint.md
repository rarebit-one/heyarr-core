# 0013. Blob serving is a contract, not an endpoint

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §28 requires every peer serving a Blob to support HTTP byte ranges, listing
direct playback, remote ffprobe, random access, resumable copying, partial
verification and worker access as the reasons.

## Decision

`GET|HEAD /api/v1/blobs/{hash}/content`, implemented with `http.ServeContent`
over a seekable reader from the CAS. Strong `ETag: "blake3-<hash>"`,
`Cache-Control: public, max-age=31536000, immutable`, `Accept-Ranges: bytes`,
`X-Content-Type-Options: nosniff`.

## Consequences

This is deliberately the *same* endpoint that Milestone 4 replication reads
from, that Milestone 6 uses as an HTTP web-seed for BitTorrent transfers (§27),
and that Milestone 2's workers probe remotely instead of materialising whole
blobs (§29). Treating it as "the playback endpoint" and optimising it for a
video player would break three later milestones.

Because blob content is immutable and hash-addressed, aggressive immutable
caching is correct rather than a risk — the URL cannot ever mean different
bytes.

Memory use must be flat in blob size. A 20 GB remux is a normal case.
