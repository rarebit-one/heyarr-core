# 0006. The CAS owns bytes; paths are not identity

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §61 names "filesystem paths as canonical identity" as one of the *arr
constraints Heyarr exists to avoid. Spec §66 brings ingested bytes under Heyarr
management.

## Decision

Filesystem paths are ingest **inputs** and operator-facing detail. They never
appear in the domain model as identity. The on-disk layout of the CAS is private
to `internal/storagefabric/cas` and versioned by a marker file, so it can change
without touching anything above it.

Enforced by `depguard`: `internal/domain/**` cannot import `os`,
`path/filepath`, `database/sql`, the persistence packages, or the CAS package.

## Consequences

Renaming or reorganising a library on disk does not disturb the catalog.
Two paths holding identical bytes are one Blob and two Assets — which is what
makes a library survive being reorganised by a previous tool.

The cost is indirection: the domain must ask for bytes through an interface it
defines. That indirection is exactly what Milestone 4 needs when "the bytes" may
live on another peer.
