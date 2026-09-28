# 0101. A vault change names the log heads it saw, a device snapshots, and compaction stays the peer's job

**Status:** Proposed
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
  subsumed. A parking rule would strand exactly those writes (§43).
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
- **Compaction stays an operator decision.** Only a peer knows which replicas
  acknowledged what (§45, the `ackedFrontier` in `store.CompactChanges`), and
  no peer can verify a snapshot it cannot decrypt. So compaction is the
  explicit `heyarr space compact` (admin), never a device capability and never
  unattended. A peer-side job may **report** what compaction would drop. It
  does not delete on its own.

## Consequences

- Once replication pushes changes in topological order, rolling out parents
  needs no coordination between devices. Old devices keep pushing roots, which
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
