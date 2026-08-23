# 0029. A second Full Peer is controller-attached and runs no control plane

**Status:** Superseded by [ADR-0038](0038-there-is-no-central-authority-peers-are-repositories.md)
**Date:** 2026-08-22

## Context

Milestone 4 adds a second Full Peer, and the obvious question — the one that has
to be answered before any code assumes an answer — is what that peer's control
plane is. Two readings were available: the second peer runs its own controller
and the two reconcile, or the second peer attaches to the one controller that
already exists.

The spec does not leave this open. §48: *"This state remains single-writer
initially. Do not attempt active-active replication of a live SQLite database."*
§52, on the peer's own local database: *"The snapshot should not be treated as
independently writable control state."* §85: *"Only the coordinated mutable
control plane initially requires a leader"*, drawn as one `SINGLE-WRITER CONTROL`
box above both peers. §80 enumerates what crosses between sites — content CAS,
chunks, encrypted state, catalog snapshot, controller backup — and control state
is not in the list.

Every "no primary / equal custodian" statement in the spec (§3, §5, §8, §30, §85)
is scoped to **content bytes and personal state**. None of them is about the
control plane, and §85 says the opposite about control.

## Decision

A Full Peer is **controller-attached**. It runs `heyarr peer` — optionally with
`heyarr worker` alongside it (§9) — and never `heyarr all`, which would
instantiate a second controller.

It holds durable local state, but none of it is control state:

- the **content CAS**, which it owns and serves (§5, §31);
- a **materialised catalog snapshot**, read-only, rebuilt from the controller
  (§52) — this milestone;
- a **controller backup stream** and **cached access leases** (§50, §54) —
  Milestone 7, not this one.

Authorisation, scheduling, placement and read routing are controller decisions
(§7, §21, §32). The peer executes; it does not decide.

Equal custodianship is about bytes. Both peers hold complete, independently
serveable replicas and neither is a backup of the other (§85) — that is fully
preserved here, because custody of bytes and authority over mutable decisions
are different questions.

## Consequences

Losing the controller degrades the deployment rather than failing it over. That
is the intended behaviour (§53) and it is Milestone 7's work, not this one; what
this milestone owes it is the snapshot that makes it possible.

A remote peer cannot write control-plane rows directly. Anything it learns that
the control plane must know — an inventory that disagrees with `replicas`, a
completed transfer, a failed verification — travels to the controller over the
API and is written there, by the single writer.

There is exactly one authoritative controller, so "which peer is right" is never
a question that has to be answered about control state. It is still a question
about bytes, and content addressing answers that one.

## Superseded

ADR-0038 replaces this. The reading below was faithful to §80 and §85 — but the
hub topology it describes was never run, and could not be: a worker claims jobs
against local SQLite, and no HTTP surface lets a remote peer claim, lease or
complete one.

Milestone 4 built every fabric primitive peer-to-peer instead, between two
independent instances, and the spec hedged twice about the leader being an
initial arrangement (§48, §85). ADR-0038 ratifies what was built: each peer is a
repository with its own control plane, and there is no node whose loss is
special.

What this record got right and ADR-0038 keeps: equal custody of bytes is about
bytes, and a single-writer database per peer is the strongest form of
Invariant 5, not a violation of it.

## Revisit if

A deployment genuinely needs to keep accepting control-plane writes while the
controller is unreachable — not reads, which §53 already covers, but writes.
That is a distributed-log problem rather than a SQLite-replication one, it would
supersede ADR-0003 as well as this record, and it should be a new ADR that says
so explicitly.
