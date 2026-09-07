# 0078. An on-demand poll reuses the poll job and never reschedules

**Status:** Accepted
**Date:** 2026-09-07
**Builds on:** ADR-0002 (roles communicate only through the job table and HTTP), ADR-0008 (durable, leased, idempotent, dedupe-keyed jobs), ADR-0017 (time, identifiers, determinism), ADR-0055/§55 (followed sources), ADR-0059 (the poll outcome is stored and a want is created through one path)

## Context

A followed source polls on a cadence measured in hours — `feed-poll` starts at
six hours and backs off toward a day for a source that keeps emitting nothing
(§55, `internal/domain/followed/schedule.go`). The follow beat enqueues a poll
only for sources that are *due* (`DueSources(now)` → `dispatch`), and a freshly
followed source's first scheduled poll is up to six hours out. The follow door
papers over the first-poll case by enqueuing one poll at follow time, but after
that there was no way to say "ask this source now": an operator who knows a feed
just posted, or an agent acting on a person's "check my podcast", could only
wait for the beat.

The manual side of this pattern already exists elsewhere and is the model to
copy, not to reinvent: `search_releases` is "search this want now" over the
search beat, and `sync_peer` is "reconcile now" over the reconciliation cycle.
Both queue the beat's own job on demand rather than doing the work inline. A
poll-now that reimplemented polling, or that advanced the schedule as a side
effect, would be a second thing that could drift from the beat.

Two questions had real answers to pick.

**What does it enqueue?** A new "forced poll" job type and handler, or the
existing `poll_source` job the beat already enqueues.

**What does it do to the schedule?** Treat the forced poll as the next
scheduled poll brought forward (advance `next_poll_at`), or as an extra poll
that leaves the cadence alone.

## Decision

**An on-demand poll enqueues the beat's existing `poll_source` job under its
existing dedupe key, and does not touch the source's `next_poll_at`.**

1. **It reuses `followed.PollSourceJobType` and `followed.PollDedupeKey`** — the
   exact enqueue the follow door runs at follow time and the follow beat runs on
   the tick. There is no forced-poll job type and no second handler. One job
   type means one worker path, so a poll asked for by an operator and a poll
   asked for by the beat cannot come to mean different things, and the poll
   outcome is still stored through ADR-0059's one path.

2. **The dedupe key is the idempotency (ADR-0008).** A source that already has a
   poll queued — from the beat, from the follow door, or from a previous
   poll-now — gets that live job back rather than a second one, over the same
   partial-unique index the beat relies on. Asking twice cannot double-poll a
   feed host, so the endpoint is safe to hammer and safe to call in a script.

3. **A forced poll is an *extra* poll, not a reschedule.** `next_poll_at` is
   left exactly as it was. Forcing a poll now is not evidence about when the
   next *scheduled* poll should fall, and the worker's `RecordPollOutcome` still
   owns the schedule and the backoff. Advancing the schedule on a forced poll
   would let an operator refreshing a feed twice quietly push its regular cadence
   around — a surprising coupling between "look now" and "look less often later".

4. **Two doors, one intent (§55).** `POST /api/v1/followed-sources/{id}/poll`
   and the MCP `poll_source` tool (write scope) both call one exported
   `resources.PollSource`, the same discipline `follow_source` and
   `want_content` are built on. A bulk `POST /api/v1/followed-sources/poll`
   sweeps every followed source through the same per-source enqueue, best-effort
   per source the way the beat's `dispatch` is. An unknown source id is a
   not-found, not a silent success.

## Consequences

- An operator or agent can force a poll immediately after following a source, or
  when a feed is known to have posted, without waiting up to six hours.
- Nothing new runs: a forced poll is the same job, worker, event and outcome a
  scheduled poll is, so it inherits the beat's routing (a node with no feed
  adapter leaves the job pending and visible, ADR-0025) for free.
- The schedule stays the beat's to own. A forced poll cannot distort a source's
  cadence, so "why did this poll at 03:14" (ADR-0017's determinism goal) still
  has one answer.
- The bulk endpoint can enqueue up to one job per followed source in a burst; on
  a large library that is bounded only by the number of sources, not by the
  beat's `followBatchLimit`. This is acceptable for an explicit operator action
  and is deduped per source, but see below.

## Revisit when

- **The bulk sweep needs pacing.** If "poll everything" against a hundred
  sources pointed at a handful of hosts becomes a thundering herd, the bulk
  endpoint should hand the work to the beat (mark sources due, or enqueue in
  batches) rather than enqueue every source inline. The per-source endpoint does
  not have this shape and does not change.
- **A forced poll should sometimes reset the backoff.** If an operator forcing a
  poll on a long-quiet source genuinely wants its cadence to return to the floor,
  that is a deliberate reschedule and should be a separate, named action — not a
  side effect folded into poll-now.
