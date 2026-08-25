# 0045. A partial holding is transfer-scoped, and is never a replica

**Status:** Accepted
**Date:** 2026-08-25

## Context

`replicas` records four states — `present`, `pending`, `not_required`,
`undecided` — and there is no `partial`. So a peer that is midway through
fetching a blob has a row indistinguishable from one that has never heard of it,
and `catalog.BlobSources`, which selects `present` replicas, returns nothing for
a blob no peer holds whole.

That is §23's opening sentence, still true in the schema, and Milestone 6 did
not change it. It **routed around** it: the piece transport takes its candidates
from membership rather than from the catalogue, asks every pinned member what it
holds over `GET /peer/v1/blobs/{hash}/pieces`, and runs *only* in the branch
where `BlobSources` returns nothing (#279). Where some peer holds the blob
whole, the streamed pull is better in every way and is untouched.

Routing around a gap is not the same as deciding the schema is right, and #288
was filed so the next person would find the reasoning rather than rediscover it.
Two things then made the question timely rather than theoretical:

- **Placement cannot see progress.** A peer 90% of the way through a transfer
  reports as holding nothing, so §56 draws `converging` and `not_satisfied` on
  information the fabric has and does not record. An operator watching a large
  transfer sees no movement at all.
- **The catalog snapshot inherits the same blind spot.** §52's read view, built
  for degraded operation, can say *not here* and never *on its way*.

## Decision

**A partial holding is recorded, in a table of its own, with the lifetime of the
transfer that produced it. It is not a replica state, and `BlobSources` never
returns one.**

### Why not a fifth `replicas` state

Because it is not the same kind of fact. `present`, `pending`, `not_required`
and `undecided` are statements about **placement** — what this fabric intends
and what it has achieved — and they are stable enough to be believed between the
moments anything looks at them. A partial holding is a statement about a
**transfer in flight**, it changes several times a second, and it is wrong
within moments of being written.

Putting a value that goes stale in seconds beside values that do not is an
invitation to read them the same way, and the code that would do the reading is
the code that decides where bytes live. ADR-0041 already draws this line for the
piece table — *"a transport detail with a session's lifetime, never an
identity"* — and this is the same line one layer up.

The concrete failure a fifth state would eventually produce: `BlobSources`
learns to exclude `partial`, somebody adds a query that forgets to, and a
streamed whole-blob pull is attempted against a peer holding a third of it. The
separate table makes that unrepresentable rather than forbidden.

### What the record is for, stated narrowly

**Visibility, and nothing else.** Placement may report it, an operator may watch
it, and the catalog snapshot may carry it as an explicitly in-flight fact. It is
never a source, never a precondition, and never evidence.

In particular it must not be consulted by:

- `catalog.BlobSources` — a partial holder cannot serve a streamed pull, and the
  whole value of #279's branch is that the two paths are told apart cheaply;
- the garbage collector's durability precondition (ADR-0018) — "somebody is
  part-way through fetching these bytes" is not "the bytes survive elsewhere",
  and a collector that accepted it would delete a last copy on the strength of a
  transfer that may fail;
- the piece transport itself, which asks peers directly and must keep doing so.
  A peer's own availability bitset is the authority on what that peer holds; a
  row in somebody else's database is a second-hand copy of it, and ADR-0038 is
  explicit that a peer is authoritative for its own site.

### Lifetime

Written by the transfer session as pieces land, deleted when the transfer
publishes, discards, or is reaped. **A row that outlives its transfer is worse
than no row**, so the reaper is part of this decision rather than a follow-up:
it rides the path that already reaps abandoned staging files by age
(`ReapTemp`), because a partial holding and the `.part` file it describes have
exactly the same lifetime and the same reason to disappear.

A crash therefore leaves rows that describe transfers nobody is running. That is
acceptable and is why the record is never evidence: the worst it can cause is an
operator seeing progress that has stopped, which the age of the row already
tells them.

## Consequences

- **Placement gains an in-flight axis it did not have**, and §56's `converging`
  can distinguish *nothing is happening* from *bytes are arriving*. That is the
  operational win and the reason this is worth any schema at all.
- **The catalog snapshot may carry it, marked as in-flight**, and must not merge
  it into the settled view. §52 already says the snapshot is not independently
  writable control state; this adds that it is not a place where a transfer's
  progress becomes a fact about placement.
- **`BlobSources` is unchanged**, which is the point. The M6 branch that sends a
  blob nobody holds whole through the swarm keeps working exactly as it does.
- **The piece transport is unchanged.** It still asks peers, and the answer still
  comes from the peer's own bitset. This record adds an observer, not a source.
- Migration numbers below the highest used (`00032`) that remain free: `00022`,
  `00026`, `00027`, `00030`, `00031`.

## What would make us revisit this

- **A fabric large enough that asking every member costs.** The survey is a round
  trip per member and no disk read, which is nothing at three peers and is a
  hundred round trips at a hundred. If this table ever becomes a plausible way to
  *narrow* the candidate set before asking, that is a real change and it is where
  the temptation to make it a source will come from. It should be refused on the
  same grounds as above, or accepted deliberately in a new record.
- **Progress turning out not to be wanted.** This is built for an operator
  watching a transfer. If nothing ever reads it, it is a table that costs writes
  and buys nothing, and deleting it is cheaper than keeping it — which is the
  advantage of it not being a replica state.
