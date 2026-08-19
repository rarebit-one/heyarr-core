# 0018. Deletion is logical; bytes are reclaimed by garbage collection

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §14 makes blobs immutable but never says when bytes may be removed. Spec
§30 distinguishes a replica from a backup from a cache.

## Decision

Deleting an Asset never unlinks bytes. A `gc_blobs` sweep reclaims blobs with no
referencing Asset that are older than a grace window, along with orphaned
temporary files. Garbage collection is `--dry-run` by default.

A blob that fails verification is moved to `quarantine/`, never deleted.

## Consequences

The dangerous version of this feature is one that deletes bytes synchronously
inside a request handler; a bug there is unrecoverable. The grace window means
a mistaken delete is reversible for as long as it lasts.

Once Milestone 4 lands, garbage collection acquires a second precondition:
it must confirm the placement policy is satisfied elsewhere before unlinking.
A "full peer" that garbage-collects the only surviving copy has failed at the
one thing it exists to do.
