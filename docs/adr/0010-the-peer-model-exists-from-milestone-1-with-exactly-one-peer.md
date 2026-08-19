# 0010. The peer model exists from Milestone 1, with exactly one peer

**Status:** Accepted
**Date:** 2026-08-19

## Context

Milestone 1 is a single machine. Milestone 4 adds a second Full Peer and is
where Heyarr becomes what it is for (§84).

## Decision

The `peers` and `replicas` tables exist in the first schema migration. The local
Full Peer is a real row with a generated ID and an Ed25519 keypair, marked
`self`. Ingest writes a `replicas` row even though there is only one peer to
write it about.

## Consequences

Milestone 4 becomes a protocol addition rather than a schema migration and a
rewrite of every query that assumed locality. The cost today is a handful of
rows that always have the same value.

The local peer identity is persisted in two places — the database and the CAS
root marker. If they ever disagree, the process refuses to start: that
disagreement is exactly how a deployment silently ends up with two peers
claiming one identity, and it is unrecoverable once replication has run.
