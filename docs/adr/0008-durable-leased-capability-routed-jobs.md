# 0008. Durable, leased, capability-routed jobs

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §75 requires jobs to be durable, retryable, lease-based, capability-routed
and idempotent where practical. Spec §61 rejects content-specific job systems.

## Decision

One `jobs` table in the controller database. Workers **claim** work by taking a
lease with an expiry, renew it with a heartbeat, and lose it if they stop.
A reaper returns expired leases to `pending` and increments the attempt count.
Retries back off exponentially with full jitter; exhausted jobs go to a terminal
`dead` state rather than looping. Idempotency is expressed as a `dedupe_key`
with a partial-unique index over live states.

There is exactly one job system, shared by every content type and every plane.

## Consequences

Every later milestone is "register a new job type" rather than "build another
scheduler". A worker that is killed mid-job costs one lease interval, not a
stuck queue. Handlers must be written to be safely re-run, because they will be.

Lease loss cancels the handler's context: a worker that has lost its lease must
stop, or two workers will do the same work.

## Alternatives rejected

- **An in-memory queue.** Loses work on restart, which for a hashing job over a
  60 GB remux is not a rounding error.
- **Cron as the scheduler.** Spec §61 rejects polling as the only integration
  model; reconciliation is a job like any other.
