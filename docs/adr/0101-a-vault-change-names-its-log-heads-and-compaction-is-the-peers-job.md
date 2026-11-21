# 0101. A vault change names the log heads it saw, a device snapshots, and compaction stays the peer's job

**Status:** Accepted (amended 2026-11-21). Accepted by the maintainer on 2026-11-21 (relayed).
**Date:** 2026-09-28
**Milestone:** M9 — Encrypted personal state (the vault drive surface)

## Context

The desktop vault-sync engine (`heyarr-kmp`, `VaultSyncEngine.push`) sends every
drive change with `parents = []`. That makes every stored change a causal root.
heyarr-kmp#71 argues that this makes §44 snapshots and compaction unreachable,
and proposes five steps: push parents, seed a cold start from a snapshot, push
snapshots, decide who triggers compaction, and migrate the existing history. It
flags the change as risky to CRDT merge semantics on a live vault holding tens of
gigabytes.

Two things that the issue does not state change the shape of the decision:

1. **Envelope parents are not merge input.** The drive CRDT (ADR-0095) keeps its
   causality *inside the ciphertext*: each `DriveChange` carries its Lamport
   `At`, its `Writer` and the `Base` head it observed, and `Drive.Apply` is
   order-independent. `protocol.EncryptedChange.Parents` only shapes the
   **log**: which changes a snapshot subsumes, what a peer is missing, and what
   compaction may drop. Filling parents in changes no merged state.
2. **Flat history is already compactable, just expensively.** With every change a
   root, `protocol.Heads(log)` is the whole log, and a snapshot whose frontier is
   those heads subsumes every change. `heyarr space rotate` already takes
   snapshots this way (`Heads(changes)`). The real cost of `parents = []` is a
   frontier, a snapshot id and a compaction request that grow with the log
   (thousands of ids per space), plus a cold start that has to fetch the whole
   log.

## Decision

**A vault device parents each change on `protocol.Heads` of the envelope log it
has pulled, the same rule the Go CLI already follows (`currentHeads`), and
never on a frontier derived from the drive's own `PosKey`s. A device pushes a
snapshot of its folded drive at those heads on a cadence, and cold-starts from
the latest snapshot plus the tail once snapshots are authenticated (#681).
Compaction stays an explicit `admin` decision on the peer's authority, never a
device capability and never unattended. Replication moves parents before children before any device sends a
parented change. Existing flat history needs no re-seed. The first snapshot,
taken at `Heads` = every root, subsumes all of it.**

- **Replication order comes first.** Today `protocol.Missing` returns changes
  sorted by id, `replicateSpace` pushes them in that order, and
  `Store.PutChange` accepts a change whose parents are absent. With roots only,
  that is harmless. With parents, a transfer that fails after a child lands but
  before its parent leaves the child as the target's head. Every retry then
  walks the child's ancestry on the source, decides the target already holds
  the parent, and never resends it. So before any device parents a change,
  `Missing` must return changes in **topological order** (parents first), and
  the push stops at the first failure. That alone guarantees a partial
  transfer never stores a child ahead of its parent. Holders deliberately keep
  **accepting** a change whose parents are absent, as `PutChange` does today.
  An absent parent is often a legitimately compacted ancestor: the snapshot
  frontier that `currentHeads` anchors on after compaction, or the older parent
  of an offline device's change that another device's snapshot has since
  subsumed. A parking rule would strand exactly those writes (§43). Order alone
  does not heal a peer that is **already** missing an ancestor, and the Go
  `space put` and `vault put` paths write parented changes today. So #682 also
  adds a repair reconcile, which compares ids the target confirms it holds and
  resends absent ancestors. Ids covered by the target's replicated pinned base
  count as held. It runs once on upgrade and then as a periodic audit. #682 is the precondition for the device change below.
- **Parents.** The engine already folds the pulled log, so it holds the envelope
  ids. The heads are the ids that no pulled change names as a parent. Only
  `push()` changes, and only its second argument. Pin it with a Go↔Kotlin parity
  vector: the same pulled log yields the same heads.
- **Snapshots from the device.** `POST /spaces/{id}/snapshots` needs `write`,
  which the daemon already holds. Push after N folded changes or on a slow timer,
  encrypted under the space key (ADR-0049). `Drive.Snapshot` is deterministic,
  and heyarr-kmp's `DriveCrdtVectorsTest` pins the Kotlin snapshot bytes per
  case. heyarr-core commits no drive vector file of its own, though. The Go
  side has only round-trip tests. So the cold-start step first commits a Go
  generated drive-snapshot fixture that both suites read. Without it the JSON
  contract (field names, `omitempty`, ordering) is pinned in one direction
  only.
- **Cold start and compaction are gated on snapshot trust (#681).** Fetching
  `getSnapshot`, folding it, and applying only the tail extends to a fresh
  device the restart case #110 covered locally. Review of this record found
  that snapshots cannot yet carry that weight. First, the frontier sits
  outside the AEAD, so a keyless `write` token can re-label valid ciphertext
  with a newer frontier. Second, the latest snapshot by arrival time may not
  cover the compaction base. Third, only the latest snapshot replicates. So a
  vault device **may push snapshots** (harmless while nothing trusts them),
  but a device **does not cold-start from a server snapshot**, and no vault
  space is compacted, until #681 has:
  - bound `(space, record type, frontier)` into the authenticated data;
  - made compaction name its snapshot by id and **pin** it as the base;
  - made cold start prefer the pinned base unless a newer authenticated
    frontier provably covers it;
  - replicated the pinned base to every trusted Full Peer (§45).
  - replaced the caller-supplied `ackedFrontier` with a durable per-peer
    acknowledgement record, so compaction is proven by what each trusted Full
    Peer confirmed holding.
- **Compaction stays an operator decision.** Only a peer knows which replicas
  acknowledged what (§45, the `ackedFrontier` in `store.CompactChanges`), and
  no peer can verify a snapshot it cannot decrypt. So compaction is the
  explicit `heyarr space compact` (admin), never a device capability and never
  unattended. A peer-side job may **report** what compaction would drop. It
  does not delete on its own.

## Consequences

- Once #682 (topological order plus ancestor repair) has shipped to the peers,
  rolling out parents needs no coordination between devices. Old devices keep pushing roots, which
  stays correct and only costs frontier size. New devices
  push parented changes, and readers already read `parents`. Nothing about
  merge semantics changes, so the "live 48 GB vault" risk lies in the snapshot
  and compaction steps, not in the parents change.
- The first snapshot on an existing space has a frontier as long as its log. It
  is a one-off cost, and that frontier collapses to a handful of heads from then
  on.
- Compaction deletes rows on a live vault, and a peer cannot tell a good
  snapshot from a hollow one. It therefore stays a human decision. The
  report-only job is the reversible step, and deletion stays out of reach of
  both automation and devices.

## Alternatives rejected

- **Parents derived from the drive's `PosKey` frontier**, as heyarr-kmp#71
  suggested. That mixes two causal layers. `PosKey`s are CRDT write keys inside
  the ciphertext, not envelope change ids, and a peer walking parents would find
  ids it has never stored.
- **A narrower `compact` scope for the daemon.** The daemon can push a snapshot
  safely. It cannot know the acknowledged frontier of every replica, so granting
  it compaction moves the data-loss decision to the party least able to make it.
- **A snapshot-and-truncate or re-seed migration.** Not needed. A snapshot at
  `Heads` already subsumes the flat history.

## What would make us revisit

- A replica topology where the peer cannot learn acknowledgements (a device-only
  vault). Then compaction needs another source of truth.
- A verifiable snapshot-eligibility rule: for example, a snapshot countersigned
  by a second device that re-folded the log and got the same plaintext.
  Unattended compaction is back on the table only once such a rule exists.
- Frontier size mattering even after the first snapshot, which would mean many
  long-lived concurrent writers.

## Relationship to existing records

ADR-0095 (the drive CRDT and its internal causality), ADR-0049 (snapshots are
encrypted under the space key), ADR-0018 (nothing is deleted inline, and the
compaction job follows the same caution), §43, §44, §45. Tracks heyarr-kmp#71,
and follows heyarr-kmp#70 and #110.

## Amendment: snapshot trust lands (2026-11-21, #681, #682)

Accepted by the maintainer on 2026-11-21 (relayed). This amendment records the
choices the decision above left open, in the order the gate lists them.

- **The frontier is authenticated** (#683, already shipped). Cold start trusts
  only a snapshot whose envelope verifies. A legacy, unauthenticated snapshot is
  never a cold-start seed, and `heyarr space compact` refuses one unless the
  operator passes `--unverified`.
- **Compaction names its snapshot, and the snapshot becomes the pinned base.**
  `POST /spaces/{id}/compact` takes `{"snapshot_id"}` and no longer accepts a
  frontier. The named snapshot is pinned as the space's compaction base (one per
  space, `snapshot_bases`). A new base must cover the old one, which the peer
  checks without a key: every head of the old base is reachable from the new
  base's frontier through held changes and the parents they name. A compaction
  only drops changes the pinned snapshot subsumes.
- **Cold start prefers the base.** `GET /spaces/{id}/snapshot` answers the
  newest snapshot whose frontier covers the pinned base. When none does, it
  answers the base itself, and with no base, the newest snapshot as before.
  `GET /spaces/{id}/snapshot/base` answers the base alone, so a device whose
  first choice fails to verify can fall back to it before it falls back to a
  full pull. The peer chooses by causal metadata, and the device verifies the
  envelope. Neither one trusts the other's half.
- **The base replicates.** After a space's changes, a reconcile pushes the base
  snapshot, then the latest, then pins the base on the target (`POST
  /peer/v1/state/{space}/base`). A target refuses a pin that does not cover its
  own base (409 `base_not_covering`). The source treats that refusal as
  "behind", the way it treats a superseded wrap. It skips the pin and does not
  fail the space. A peer offers its base's frontier alongside its heads, so a
  sibling's heads-based push never resends what that peer compacted.
- **Acknowledgements are durable and confirmed by the target.** A new peer
  route, `POST /peer/v1/state/{space}/held`, answers which of the offered change
  ids the target holds as rows, and the target's pinned base. The reconcile's
  audit counts as confirmed every id the target holds, plus every id in the
  causal history of the target's base frontier, walked over the source's own
  log. It resends the rest parents first (#682's repair), and only when every
  push succeeded does it record `state_peer_acks(space, peer) = (Heads(log),
  base)`. A target's reported heads are never taken as proof.
- **Compaction is proven by those acks.** A change is dropped only if the pinned
  snapshot subsumes it and it is in the acked causal history of **every**
  trusted Full Peer whose ack names this same base. While any peer lacks such an
  ack, the call pins the base, drops nothing, and lists the peers it is waiting
  for. Running it again after the next audited reconcile does the drop. A node
  with no trusted Full Peers has no one to wait for. That stays as before: its
  log is the only copy, and the base makes it recoverable.
- **When the audit runs.** Every on-demand reconcile (`POST
  /api/v1/state/replicate`) audits. The beat audits on its first pass after
  start, which is the "once on upgrade" run, and then hourly, so acks lag
  replication by at most an hour. A pin or audit failure is logged and recorded
  as a deferral. It never fails the space's replication: a peer that predates the
  routes still receives every change and snapshot, it just never acks, and
  nothing is compacted against it.

What would make us revisit: a repair whose id lists outgrow the request bound in
practice (then send a range digest instead of ids), or a deployment that needs
compaction to wait for fewer than every trusted Full Peer.
