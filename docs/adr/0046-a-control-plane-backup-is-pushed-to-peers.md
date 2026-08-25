# 0046. A control-plane backup is pushed to peers; content is pulled by them

**Status:** Proposed
**Date:** 2026-08-25

## Context

§50 replicates a peer's controller backup to every trusted Full Peer. The
direction of that transfer is a decision, and it is not automatically the one
ADR-0030 already made.

ADR-0030 settled that **content replication is a destination pull**: the
controller never carries blob bytes, because a blob is large and the controller
coordinates convergence without owning the bytes. The catalog snapshot follows
the same shape — a peer **pulls** it from the controller (`internal/peer/catalog/refresh.go`).

So the surface already has a pull idiom, and reusing it would be the path of
least resistance. This record says why the control-plane backup goes the other
way instead.

## Decision

**A peer PUSHES its own control-plane backup to each trusted Full Peer.** The
content plane keeps pulling; the control-plane backup does not.

### Why ADR-0030's reasoning does not transfer

ADR-0030 optimises away *the controller carrying bytes it does not need to own*.
That premise is absent here: a control-plane backup **is the controller's own
database**. The bytes are its state, it produced them, and it already holds
them — there is nothing to avoid carrying. They are also small (a homelab
control plane, not a media library), so the cost ADR-0030 exists to remove is
not a cost here. The decision that made content a pull says nothing about an
artefact the source inherently owns.

### Why push, positively

1. **The source knows the instant a new backup exists** — it just took it (§49
   cadence). A push propagates at the RPO cadence; a pull waits for each peer's
   next poll, widening the window in which a peer holds a stale generation.
2. **The artefact is the whole control plane** — grants, membership, policy.
   *Who* receives it is a trust decision, and the source is the party that holds
   the trust (its own membership records, ADR-0012). Push makes the trust
   boundary the source's to enforce, rather than something each receiver decides
   to reach for.
3. **It is structurally opposite the catalog snapshot's pull.** §50's
   load-bearing point is that the backup is *not* the catalog snapshot (§52).
   The snapshot is pulled by the peer from the controller; making the backup a
   *push from* the controller means the two artefacts move in opposite
   directions on the same surface — one more way they cannot be confused, on top
   of separate files and the `goose_db_version` marker (ADR-0044 Q5).
4. **The distribution cycle is naturally the source iterating its own
   membership** — push to every trusted Full Peer, make progress with whoever is
   reachable, never block on a dead one (ADR-0041's progress rule, ADR-0037's
   "unreachable is ordinary"). That is a loop the source owns end to end.

### Revocation, both ends

Because the source **re-reads its membership each cycle**, a peer revoked
mid-cycle — its membership record deleted (ADR-0012) — drops out of the push set
before the next push. That is the primary mechanism, and it is the source's.

The receiving side is belt to that brace: the peer surface already rejects an
inbound client whose certificate is not pinned in membership, so a push in
flight to a peer revoked a moment ago fails the mTLS handshake rather than
landing. Neither end trusts the other to have noticed the revocation first.

## Consequences

- A new authenticated endpoint on the peer surface accepts a pushed backup and
  stores it as a **held backup** — inert, refused as a control plane (ADR-0044
  Q5), in its own directory, never the catalog snapshot's.
- **Retention stays on the receiver.** Push decides *when* a backup is sent, not
  *how many* a peer keeps — a peer that kept only the newest would have nothing
  when the newest is the one written during the incident. How many, and why, is
  the receiver's policy.
- The source tracks *what it believes each peer holds*; the receiver tracks what
  it actually holds. When they disagree the receiver's answer is the fact — the
  M4 durability lesson (`internal/peer/durability`) that a controller's belief
  about a machine it is not is a belief.

## What a pull would have bought, and what would make us revisit

A pull would give each receiver control of its own cadence and reuse the
`refresh.go` machinery. We keep the half that matters — retention is the
receiver's either way — and give up only the receiver choosing *when*, which the
source's RPO already answers better.

**Revisit if** the trusted-Full-Peer count grows past what a source can push to
serially within a cadence (then a gossip fan-out, or a pull), or if a deployment
genuinely needs peers to hold backups on a schedule decoupled from the source's
RPO.
