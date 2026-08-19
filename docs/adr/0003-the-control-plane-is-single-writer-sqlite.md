# 0003. The control plane is single-writer SQLite

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §48 separates control-plane state (desired content, policy, leases, grants,
peer membership, provider config) from convergent content state and from
encrypted personal state, and says explicitly: *do not attempt active-active
replication of a live SQLite database.*

## Decision

The controller owns one SQLite database with a single writer. WAL mode,
`foreign_keys=ON`, `synchronous=NORMAL`, a `busy_timeout`, and periodic
checkpointing. No multi-writer, no active-active, no distributed consensus in
ordinary domain operations.

## Consequences

Resilience comes from **backup streams to Full Peers** (§50) and restore
tooling (§51), not from replication. A surviving peer rebuilds a new
authoritative controller; it does not silently become one.

This is the only plane that requires a leader. Content converges through
immutable content addressing and personal state converges through encrypted
CRDTs — neither needs this database to be available to keep working, which is
what makes degraded operation (§53) possible at all.

## Revisit if

A deployment genuinely needs concurrent controllers. That is a distributed-log
problem, not a SQLite-replication problem, and would be a new ADR.
