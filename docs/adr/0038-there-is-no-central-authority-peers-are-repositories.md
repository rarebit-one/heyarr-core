# 0038. There is no central authority; peers are repositories

**Status:** Proposed
**Date:** 2026-08-23

## Context

The spec describes one controller above two Full Peers (§80, §85), and ADR-0029
recorded that reading: a Full Peer is controller-attached and runs no control
plane of its own.

Milestone 4 built something else. Its two-peer acceptance run stands up two
independent `heyarr all` instances — two catalogues, two desired-state tables,
two job queues — that enrol each other by pinned Ed25519 key and replicate blobs
between them. Every fabric primitive was built and proven that way: identity,
membership, mTLS, inventory exchange, `reconcile_peer`, destination-pull
transfer, and the GC durability precondition.

The hub topology has therefore **never been run**, and cannot be today: a worker
claims jobs through `worker.Queue` against local SQLite, and the HTTP API offers
`GET /jobs`, `GET /jobs/{id}` and `POST /jobs/{id}/retry` — no claim, no lease,
no completion. A remote peer attached to a distant controller has no way to do
work.

Two further observations pushed the question:

- **The spec hedged, twice.** §48: *"This state remains single-writer
  initially."* §85: *"Only the coordinated mutable control plane initially
  requires a leader."* The qualifier is deliberate in a document that is
  otherwise unhedged.
- **Asymmetric reachability broke the hub, not the fabric.** With one host able
  to reach another but not the reverse, replication deadlocked — inventory is
  *pushed* to a controller while bytes are *pulled* from a peer, so whichever
  host was the destination, one required flow ran the wrong way. That is a
  consequence of splitting the roles across a network, not of replication.

## Decision

**A peer is a repository.** Each holds a complete control plane of its own —
catalogue, desired state, policy, jobs, membership — in its own single-writer
SQLite database. There is no controller above them and no node whose loss is
special.

Peers exchange with each other the way git repositories do:

- **Objects are immutable and content-addressed.** The BLAKE3 whole-object digest
  remains the only identity (ADR-0005, Invariant 1). Blobs converge without
  coordination because identical bytes are the same object everywhere.
- **You fetch what you are missing from whoever you can reach.** Reachability is
  a property of a pair, not a property of the deployment. A peer that can be
  reached but cannot reach back is an ordinary participant, not a broken one.
- **Trust is per-repository.** Membership is what *this* peer will talk to.
  Enrolment and revocation are local decisions with local effect.
- **Divergence is normal and must be representable.** Two peers may want
  different things and be right.

### This satisfies Invariant 5 rather than violating it

"Never active-active SQLite" forbids two writers on one database. Giving each
peer its own single-writer database is the strongest possible form of that rule
— stronger than a remote peer reaching across a network to a single one.
ADR-0003's decision stands unchanged; what changes is its *consequence*, which
assumed one database in the deployment rather than one per peer.

## Consequences

**ADR-0029 is superseded.** A Full Peer is not controller-attached; it *is* the
control plane for itself. Its catalog snapshot (§52) stops being a degraded-read
cache of somebody else's catalogue and becomes what it always was locally.

**Milestone 7 changes shape and gets smaller.** "Losing the controller stops
being fatal" presumes a controller whose loss matters. In a peer-repo model
losing a peer costs that peer's local state, and recovery is re-cloning from a
peer that still has it — which is a fetch, not a restore. Continuous SQLite
backup to peers (§49, §50) and `heyarr recover --from-peer` (§51) are still
useful, but as *convenience over a fetch*, not as the difference between working
and not.

**Three things get harder, and they are the ones git never had to solve, because
git has no runtime authorisation.**

1. **Revocation is per-repository.** Removing a membership record at peer A does
   not remove it at peer B. "Revoked" stops meaning "revoked everywhere", and an
   operator revoking a compromised key must do it at every peer that holds it.
   This must be said where the operator meets it, not only here.
2. **Grants and capabilities (M8, M9) inherit that.** A capability honoured on
   one peer and revoked on another is a security surprise rather than a
   convergence detail. M8 must decide whether grants are per-repository facts or
   whether delegations carry something that makes revocation propagate.
3. **Desired state can conflict.** Two peers wanting different quality profiles
   for one work is a genuine disagreement. Either a merge model, or an explicit
   stance that desired state is per-repository and does not merge — but not
   silence.

**Acquisition may be duplicated.** Two peers independently wanting the same
missing content will each go and get it. That is wasteful rather than wrong, and
it is exactly what M6's cooperative acquisition (§23, §24) exists to collapse.

**One-way reachability stops being a deadlock.** The two flows co-align: a peer
fetches what it lacks from a peer it can reach. The open question shrinks from
"can this topology work at all" to "which direction does inventory travel", which
is answerable without an architecture change.

## What this does not change

The content plane and the personal-state plane are already leaderless by design
(§5, §8, §43) and are untouched. So is every M4 primitive: pinned mTLS,
membership as the trust root, destination-pull with independent verification, and
a GC that refuses to unlink without proving the bytes survive elsewhere. Those
were built peer-to-peer and remain correct.

## Revisit if

A deployment needs a decision that genuinely cannot be made locally and cannot
tolerate divergence — a global uniqueness constraint, or a policy that must be
provably identical everywhere at an instant. That is a consensus problem, it is
not what a homelab content fabric needs, and it would be a new ADR that says so.

Equally: if per-repository revocation proves to be a footgun in practice rather
than in theory, the answer is probably signed, expiring grants (M8) rather than a
return to a central authority.
