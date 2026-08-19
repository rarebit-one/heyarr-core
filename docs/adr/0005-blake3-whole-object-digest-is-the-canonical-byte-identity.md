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

Implementation: `zeebo/blake3`, chosen for its hand-written AVX2/AVX-512 and
NEON assembly. This is the throughput-critical dependency, so the repository
carries its own benchmark rather than trusting a README.

## Consequences

Exact deduplication, corruption detection, replica verification and
location-independent addressing all fall out of one property. A destination
always verifies bytes itself (§21); it never trusts a source's claimed hash.

The cost is that any mutation rewrites identity. That is the point: it is what
makes "no primary content replica" (§8) coherent.
