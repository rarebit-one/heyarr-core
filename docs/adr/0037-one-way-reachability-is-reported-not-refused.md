# 0037. One-way reachability is reported at enrolment, never refused

**Status:** Accepted
**Date:** 2026-08-23

## Context

Replication needs two flows, and in Milestone 4 they ran in opposite
directions: inventory was **pushed** from a peer to a controller
(`POST /peer/v1/inventory`), and bytes were **pulled** by a destination from a
source. On a network where reachability is symmetric that asymmetry is
invisible. On one where it is not, whichever host was the destination had
exactly one of the two required flows running the wrong way, and replication
deadlocked with no error anywhere: reconciliation simply emitted nothing,
correctly, because nothing had told it where the bytes were.

That was observed on real hardware between two hosts on different subnets.

The first version of this record concluded that one-way reachability was
therefore **unsupported**, and refused such a pairing at enrolment. Its
load-bearing argument was job claiming: a remote worker cannot claim or lease
work over HTTP, so the number of flows needing the peer → controller direction
was going up rather than down.

**[ADR-0038](0038-there-is-no-central-authority-peers-are-repositories.md)
removed that premise.** Each peer is authoritative for its own site and holds
its own control plane, so there is no peer → controller direction to run the
wrong way. A peer claims its own jobs against its own database. The two flows
co-align: a peer fetches what it lacks from a peer it can reach.

## Decision

**A one-way pairing is enrolled, and the direction that did not answer is
reported.** It is never refused.

Under ADR-0038 a peer that can be reached but cannot reach back is an ordinary
participant. It can fetch what it lacks from the peer it can reach, and it
keeps serving everything already on its own disk regardless. Refusing to enrol
it would block a working configuration on the grounds that it is unusual.

What survives from the first version, and why each part is still worth having:

- **The probe itself.** Knowing at `heyarr peers add` that the return leg does
  not answer is real operational information. It is a fact, not a fault, and it
  is far better learned at enrolment than inferred weeks later from a
  convergence that never completes.
- **Three outcomes, not two.** `reachable + unreachable` is now *reported*;
  anything else stays `unproven` and is silent. A powered-off peer is
  indistinguishable from a one-way network from this side, and a peer that is
  merely not up yet is the common case during enrolment — enrolment is two
  commands, and between them the far node has not enrolled you.
- **The return leg is a TCP connection, not a request.** For that same reason:
  during the first command the far node has no membership record for the
  caller, so a credentialled probe would be refused by the very peer the
  enrolment is about to make reachable. Plain HTTPS is wrong too — a peer's
  mTLS endpoint never answers it.

## Consequences

Enrolment always succeeds where it did before, and additionally succeeds for a
one-way pairing. Nothing that previously worked changes.

`peers add` gains an advisory line naming the direction that did not answer.
An operator who sees it can act on it or ignore it; the deployment works
either way.

**There is no escape hatch, because there is nothing to escape.** The first
version needed `--skip-reachability-check` to get past its own refusal. A
report needs no override, and removing the flag removes a switch whose only
purpose was to disable a check that no longer blocks anything.

The open question this record does **not** answer is which direction inventory
should travel under ADR-0038. That is now a narrow question about a single
flow rather than a question about whether the topology can work at all, and it
belongs with the peer-sync work rather than here.

## Revisit if

Inventory exchange becomes symmetric or pull-based, at which point the probe
may have nothing left to report and could be dropped entirely.

Or if an operator survey shows the advisory line is routinely ignored in
deployments where it mattered — in which case the answer is a better message,
or surfacing it in `peers list` as well, not a refusal.
