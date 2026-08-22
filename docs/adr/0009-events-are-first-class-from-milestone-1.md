# 0009. Events are first-class from Milestone 1

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §76 defines an external event stream over SSE. It would be easy to defer it
until something needs to watch.

## Decision

Events ship in Milestone 1. Every state transition appends to a monotonic,
replayable event log and is published to subscribers. `GET /api/v1/events`
supports `after=<seq>` so a client can reconnect without gaps or duplicates.

## Consequences

Spec §61 names "polling as the only integration model" as an *arr failure. The
practical reason to do it now, though, is narrower: retrofitting events means
auditing every mutation site in the codebase, and that audit gets more expensive
with every milestone. Emitting from the first mutation costs about a day.

It also pays for itself immediately — the CLI's `--wait`, the acceptance
script's assertions, and all of Milestone 4's replication observability come out
of it for free.

A slow subscriber must never block a publisher: buffers are bounded and slow
consumers are dropped with a warning, not backpressured.

The corollary, which Milestone 4 makes load-bearing: "every state transition"
is not "every blob". Replication reports cycles and transfers, never one event
per blob — the reasoning, and the rule for anyone adding a replication beat, is
recorded under "No per-blob events during replication" in the `internal/events`
package doc.
