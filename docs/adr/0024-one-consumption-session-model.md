# 0024. One ConsumptionSession model for watching, listening and reading

**Status:** Accepted
**Date:** 2026-08-20

## Context

Spec §67 lists consumption as watching, listening, reading, continuing and
queueing, and gives them one abstraction: `ConsumptionSession`.

The tempting reading is that this is shorthand and the implementation wants
three models. Watching a film, listening to an album and reading a comic feel
like different activities, and their progress units genuinely differ: a media
timestamp, a track offset, a page number or an EPUB CFI.

## Decision

One model, one table, one state machine, one event vocabulary.

The variation between the three is **one field**. Everything else — the states,
the legal transitions, the resume query, the `playback.*` events, and the
personal-state sync protocol that will eventually carry this (§44) — is
identical. Building three of each to accommodate one field is how a system ends
up with three sync protocols to keep consistent, which is the same failure the
`works.attributes` column exists to avoid: variation in one field is not
variation in shape.

So progress is a **locator and a unit**, never a float. A page number is not a
number of seconds, and a schema that stores both as `position` forces every
reader to carry a second field saying how to interpret it — which is this pair,
with the type safety removed.

**Continuing and queueing are not states.** Continuing is a *query* — the most
recent non-terminal session, which is what a "Continue watching" row is made of
— and queueing is an ordered set of intended sessions. Modelling either as a
state would allow a session to be "queueing", which is not something a playback
does; it is something a client does with a list.

## Consequences

A reader and a player are the same object to the API, the event stream and
whatever syncs them later. `GET /consumption/sessions?state=resumable` answers
"what was I in the middle of" across every content type at once, which is the
question a home screen actually asks and which three tables would answer three
times and then have to merge.

The state machine can be a pure function in `internal/domain/playback` with no
database in the way, so the whole (state, transition) space is enumerable in a
test — 5 states × 6 transitions, legal and illegal. A transition table with only
the legal half tested is half a state machine, and the illegal half is the half
that becomes a 409 nobody implemented.

Some states are trivial for some verbs. A reader never meaningfully "plays": it
passes through `playing` to `paused` on its first page turn and lives there.
That is a small cost paid in exchange for not having a second state machine,
and it is visible rather than hidden — the transition is real, it emits, and
the history reads correctly.

**A consumption session is control-plane state, not personal state.** Ratings,
annotations and the encrypted CRDT plane are §37–47 and Milestone 9. This table
records which device consumed which asset, in what state, and where it reached.
If it starts accumulating things that belong in an encrypted space, M9 becomes
a migration out of here rather than an addition alongside it.

## What would make us revisit

A consumption verb whose **state machine** differs, rather than its progress
unit. Live playback with no resume point, or something with no terminal state,
would not fit the table above and should not be forced into it.

Evidence that the resume query needs a different index per verb at real library
sizes — at which point one table with a partial index per verb is still likely
to beat three tables, but the assumption would deserve measuring rather than
asserting.
