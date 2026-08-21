# 0027. Acquisition state is four facts, not one ordinal

**Status:** Accepted
**Date:** 2026-08-21

## Context

Spec §64 draws the acquisition state machine as twelve boxes in a single
column:

```
MISSING → SEARCHING → CANDIDATES_FOUND → SELECTED → QUEUED → DOWNLOADING
→ VERIFYING → INGESTING → AVAILABLE → CONTENT_SATISFIED
→ PLACEMENT_CONVERGING → FULLY_SATISFIED
```

Read as drawn, that is one enumerated column and the obvious implementation is
one column holding one of twelve values, or an integer counting 0 to 11.

§56 says something the picture does not: satisfaction is evaluated at **two
levels, separately** — content satisfied, and placement satisfied — and the
epic for this milestone says plainly that obtaining usable content and
replicating it to every required peer "are different questions and must not be
collapsed".

## Decision

**Store four independent facts. Derive §64's name from them, and never store
the name.**

```
phase      where the acquisition pipeline is
managed    whether Heyarr holds bytes for this want
content    do we hold bytes the quality profile accepts?     (§56)
placement  are those bytes on every Full Peer that should?   (§56)
```

The last four of §64's boxes are not four pipeline positions. They are one
pipeline position — idle, holding bytes — annotated with the answers to §56's
two questions:

| name | phase | managed | content | placement |
|---|---|---|---|---|
| `MISSING` | idle | no | — | — |
| `AVAILABLE` | idle | yes | unknown / not_satisfied | — |
| `CONTENT_SATISFIED` | idle | yes | satisfied | unknown / not_applicable |
| `PLACEMENT_CONVERGING` | idle | yes | satisfied | converging |
| `FULLY_SATISFIED` | idle | yes | satisfied | satisfied |

**There is no `missing` phase and no `available` phase.** Both of those names
mean "nothing in flight" and differ only in whether bytes are held.

**The §64 name is a presentation, and nothing branches on it.** The moment
something does, the four facts have a single ordinal in front of them again and
the collapse is back.

## Consequences

**An ordinal cannot express regression on one axis.** Obtaining a file and
replicating it are different work, done by different subsystems, at different
times, and either can regress without the other — a peer can go away long after
the bytes arrived. Moving from `FULLY_SATISFIED` back to `PLACEMENT_CONVERGING`
is a decrement through a state that means something else entirely, and the code
that reads "state < AVAILABLE means we do not have it" is then wrong.

**Separating `managed` from `phase` was not in the first draft, and its absence
was a real bug.** A monitored want whose content already satisfies re-enters
`SEARCHING` for an upgrade (§60). With `MISSING` and `AVAILABLE` as phases, a
(phase, transition) table has nowhere to send a fruitless upgrade search except
`MISSING` — because the table cannot know whether bytes are held. Every
unsuccessful upgrade scan therefore reported a perfectly good library as
missing, and the next pass would try to acquire what was already on disk.

The package's own upgrade test found it before any of it shipped, which is the
argument for writing the "does an upgrade search lose the library" test at the
same time as the machine rather than after.

**The axes have different value sets, and the asymmetry is deliberate.**
`converging` is meaningless for content: there is no partial file that
half-satisfies a quality profile. `not_applicable` is meaningless for content:
content satisfaction is the whole question a DesiredItem asks.

**`unknown` is not `not_satisfied`, on either axis.** "Nobody has looked" and
"we looked and the answer is no" lead to different actions, and collapsing them
makes a fresh want indistinguishable from one just found wanting.

**ADR-0020's blob-less assets get `not_applicable`, and this is the fifth place
they need a special case.** A `linked` asset has no blob, so there is nothing to
replicate and placement is not a question that can be answered about it. Calling
it satisfied — zero required blobs are all present, vacuously true — would make
`FULLY_SATISFIED` mean "one copy, on one disk, with no integrity guarantee and
no way to verify it", which is the opposite of what the name promises. So a
linked asset rests at `CONTENT_SATISFIED` permanently, which is honest and
needs no new name.

**Placement is UNPROVEN.** ADR-0010 puts exactly one peer in the model by
design, so with a target set of one this axis is satisfied the moment content
is, and `PLACEMENT_CONVERGING` — the state this entire distinction exists to
express — is unreachable outside a test with a synthetic peer set. It is
modelled fully, unit-tested, and labelled unproven in the code, the OpenAPI and
the tests. Milestone 4 stands up the second peer and proves it.

**The name has to be computed everywhere it is shown**, including in the API,
the CLI and any future MCP tool. That is a small ongoing cost, paid to keep the
storage honest, and it is cheaper than the migration that un-collapses an
ordinal after clients have been built against it.

## What would make us revisit

A third satisfaction axis — durability or freshness, say — which would make the
derivation table large enough that a name registry beats a switch.

Evidence that clients want to filter by the §64 name in a query, which a
derived value cannot index. The answer would be a generated column rather than
a stored one, but it would be worth writing down.

A second peer actually running (Milestone 4), which turns the placement axis
from modelled to proven and may show that `converging` needs more structure —
how many peers, which ones — than a single enum value carries.
