# 0038. Each peer is authoritative for its own site

**Status:** Accepted
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

**Each peer is authoritative for its own site, syncs with the peers it can
reach, and does not mind when that fails.**

Each holds a complete control plane of its own — catalogue, desired state,
policy, jobs, membership — in its own single-writer SQLite database. There is no
controller above them and no node whose loss is special.

The three clauses are the whole decision, and each carries weight:

**Authoritative for its own site.** Not "no authority" — authority that is local
and total. A peer is the truth about what its site holds, wants and trusts.
Nothing overrides it and it overrides nothing.

**Syncs with the peers it can reach.** Reachability is a property of a pair, not
of the deployment. A peer that can be reached but cannot reach back is an
ordinary participant.

**Does not mind when sync fails.** A failed sync is not a fault condition. It is
not an alarm, not a degraded mode, and not a state to recover from — it is
Tuesday. A peer that has not heard from another in a week is working correctly.

Peers exchange the way git repositories do:

- **Objects are immutable and content-addressed.** The BLAKE3 whole-object digest
  remains the only identity (ADR-0005, Invariant 1). Blobs converge without
  coordination because identical bytes are the same object everywhere.
- **You fetch what you are missing from whoever you can reach.** Reachability is
  a property of a pair, not a property of the deployment. A peer that can be
  reached but cannot reach back is an ordinary participant, not a broken one.
- **Trust is per-repository.** Membership is what *this* peer will talk to.
  Enrolment and revocation are local decisions with local effect.
- **Divergence is not conflict.** Two peers wanting different things are two
  sites with different policies, and both are correct. There is nothing to
  merge because there was never one answer.

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

**Two things that look like problems are not, once authority is local.**

**Per-site revocation is the design.** Removing a membership record at peer A
does not remove it at peer B, and that is correct: each site decides who it will
talk to. What it costs is an operator expectation — "revoked" means "revoked
here" — and that must be said where the operator meets it, in `peers remove`,
not only in this record.

**Divergent desired state is not a conflict.** One site wanting 1080p and
another wanting 4K for the same work are two policies, both authoritative where
they live. No merge model is needed, and building one would impose an agreement
the model does not require.

**One thing is genuinely hard, and it is the one git never had to solve.**

**Grants and capabilities spanning sites (M8, M9).** A *user* is not a site. When
one identity authorises many devices across many peers, a capability revoked at
one site and honoured at another is a security surprise rather than a policy
difference — the distinction that makes divergent desired state fine is exactly
the distinction that does not apply here. M8 must decide whether a grant is a
per-site fact or whether delegations carry expiry that bounds the window. Signed,
expiring grants are the likely answer, because they make staleness self-limiting
without requiring anyone to be reachable.

**"Degraded" mostly stops being a state.** §53 models what a peer can do while
the controller is unreachable. With no controller, and with a failed sync not
being a fault, there is no undegraded state to fall from — a partitioned peer is
simply a peer. What survives of §53 is a narrower and more honest question: what
can a peer do while it cannot reach its peers? Browse, search, stream and read
its own library, all of which it could always do. What it cannot do is *prove*
anything about elsewhere.

**And that is exactly why GC's durability precondition matters more, not less.**
ADR-0018's second precondition (M4-12) refuses to unlink unless it can
affirmatively establish the bytes survive elsewhere. Under this model a peer will
routinely be unable to establish that — and it must keep refusing, silently and
without alarm. A GC that treats "cannot reach my peer" as a fault would be
noisy; one that treats it as permission to delete would be catastrophic. It
already behaves correctly: it fails closed, and `peer_unreachable` is a recorded
reason rather than an error.

**Peer health means something narrower.** An unreachable peer is not unhealthy —
it is unreachable, which is unremarkable. #184's liveness work should record
reachability as a fact with a timestamp, not as a health verdict, and nothing
should page anyone about it.

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

## What ratified this

**Accepted 2026-08-25, on evidence rather than on argument.** It was `Proposed`
for two days while Milestone 6 was built entirely on top of it, which is a
worse state to be in than either accepting or rejecting it.

Every M6 mechanism assumes this record and none of them would be correct
without it:

- **Piece candidates come from MEMBERSHIP, not from a controller's catalogue**
  (`internal/worker/replicateblob.go`). There is no node to ask who holds what;
  each peer asks the peers it can reach.
- **"A smaller swarm, not a failure"** (ADR-0041, decision 2). A pair with no
  working direction is ordinary, which is this record's third clause.
- **A session makes progress with whoever it has**, and completion is the
  target digest rather than a quorum — asserted, and the tests fail when a
  survey failure or a fetch failure is made fatal (#280).
- **The acceptance demo runs three peers with three control planes**, and the
  hub topology has still never been run.

So the code has been treating this as accepted since Milestone 4. Marking it so
records reality; it does not decide anything new.

**What this ratification obliges.** Two of the Consequences above rewrite work
that was already planned: Milestone 7 gets smaller (see #26), and "degraded"
mostly stops being a state (§53). Both had been decomposed against the
superseded ADR-0029 reading and have been reworked.

**One clause above is a prediction the code contradicts, and ratifying this
record does not ratify it.** The Milestone 7 consequence says recovery is
*"re-cloning from a peer that still has it — which is a fetch, not a restore"*.
There is no such fetch. Content arrives on a peer exclusively through
desired-state reconciliation driven by that peer's OWN control database:
`canonicalBlobs` reads local `assets`, `PlanPeerConvergence` diffs against local
`replicas`, and `reconcile_peer` enqueues one `replicate_blob` per gap. The peer
surface serves per-hash and there is no bulk route, no `clone` verb and no
`bootstrap` verb anywhere. **A node with no control plane computes zero gaps and
therefore fetches nothing** — it has nothing that decides what to want.

So the dependency runs the other way from what that clause assumes: content and
encrypted personal state do converge on their own, but they converge *toward a
desired set that only a restored control plane provides*. Backup and restore of
the control plane are therefore load-bearing rather than a convenience, and the
milestone is smaller for different reasons than this record gave — §53, not §49.

The one §82 input that genuinely is pullable in one shot is the **catalog
snapshot**: a peer whose store was lost reports `holding=0` and receives a full
rebuild. That is the read view, and it carries no desired items, policy or
grants.

**What would still make us revisit it.** A deployment where one site is
genuinely subordinate to another — a family member's node that should not be
authoritative about anything — is the case this model has no answer for, and it
is not exotic. It would not restore the hub; it would need a notion of a peer
that is a member without being an authority, which is a different record.
