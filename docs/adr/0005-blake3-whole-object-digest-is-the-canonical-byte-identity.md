# 0005. BLAKE3 whole-object digest is the canonical byte identity

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §13 requires each Blob to have a whole-object cryptographic digest and
recommends BLAKE3. Spec §15 adds content-defined chunking for efficient
replication.

## Decision

A Blob's identity is `blake3:<64 lowercase hex>`, the digest of the whole byte
sequence. Chunk manifests are an optimisation for transfer and deduplication;
they are **never** an identity. Blobs are immutable (§14): retagging a file
produces a different Blob, and an Asset may point at the new one while remaining
the same semantic object.

Implementation: `zeebo/blake3`. This is the throughput-critical dependency, so
the repository carries its own benchmark rather than trusting a README.

**Measured** (`go test -bench . ./internal/hashing`), single core:

| Platform | In-memory | From file |
|---|---|---|
| darwin/arm64 (M-series, dev) | 621 MB/s | 550 MB/s |

That is below the ≥1 GB/s originally assumed, and the assumption was for amd64
where the AVX2 assembly applies; the arm64 path is slower. It does not change
the decision, because the practical ceiling is the disk: a spinning drive
delivers 150–250 MB/s, so on an HDD-backed library hashing is not the
bottleneck. It matters on NVMe, where it is, and it is the first thing to
re-measure if ingest is slower than expected.

## Consequences

Exact deduplication, corruption detection, replica verification and
location-independent addressing all fall out of one property. A destination
always verifies bytes itself (§21); it never trusts a source's claimed hash.

The cost is that any mutation rewrites identity. That is the point: it is what
makes "no primary content replica" (§8) coherent.
