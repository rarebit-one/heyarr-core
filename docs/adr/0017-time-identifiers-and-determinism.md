# 0017. Time, identifiers and determinism

**Status:** Accepted
**Date:** 2026-08-19

## Context

Leases, backoff, grace windows and expiry are load-bearing (§75, §54). Tests
that assert on them with `time.Sleep` are slow and flaky in equal measure.

## Decision

A `Clock` interface is injected everywhere time is read; nothing calls
`time.Now()` directly outside the composition root. Entity identifiers are
UUIDv7. All timestamps on the wire are RFC 3339 in UTC.

## Consequences

Lease expiry, backoff schedules and grace windows become ordinary unit tests
with a fake clock, which means they are actually tested rather than
approximately tested.

UUIDv7 is time-sortable, so it indexes well and gives natural creation ordering
without a second column. Blobs are exempt: they are keyed by their hash, which
is their identity (ADR-0005).
