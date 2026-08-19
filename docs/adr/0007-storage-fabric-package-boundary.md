# 0007. Storage Fabric package boundary

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §17 and §18 name the content-addressed storage subsystem the *Heyarr
Storage Fabric* and require it to remain isolated behind stable interfaces,
noting it may eventually become reusable independently of Heyarr.

## Decision

`internal/storagefabric/` is a self-contained subsystem exposing interfaces:
`cas`, `chunking`, `manifests`, `replication`, `torrent`, `placement`,
`integrity`, `transport`. It may not import `internal/domain` or `internal/api`.
Enforced by `depguard`.

## Consequences

Extraction into its own module later is a `git mv` and a `go.mod`, not a
rewrite. More immediately, the boundary forces the fabric to be described in
terms of blobs, chunks, peers and policies rather than movies and episodes —
which is what lets one implementation serve thirteen content types.

Extraction is explicitly **not** required now (§18). The boundary is what
matters; the module split can wait for a second consumer.
