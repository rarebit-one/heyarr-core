# 0095. A vault is a filesystem, stored as a path-keyed-map CRDT, and a concurrent clash becomes a conflicted copy

**Status:** Proposed
**Date:** 2026-09-15
**Milestone:** M9 — Encrypted personal state (the vault drive surface)

## Context

ADR-0021 decided *what a vault is made of*: an EncryptedSpace whose members are
content, whose bytes are ordinary Blobs that happen to be ciphertext, whose
catalog is encrypted CRDT state in the personal-state plane, and about which the
control plane holds nothing. ADR-0049 decided *how its key is held*: one space
key, wrapped per device X25519 key and for the recovery key, unwrappable by no
peer. Between them they make a personal drive — replicated across sovereign
peers, unreadable to the operator — a coherent thing to build.

What neither decided is the **shape of that catalog**. A vault holding a person's
files is a *filesystem*: a mutable hierarchy of named paths, edited concurrently
from a laptop and a phone that are sometimes offline, that must converge to one
state without a coordinator (§43). The personal-state CRDTs that exist —
`playlist` and `starred` (OR-Sets, §37), `readingpos` (an LWW register per
publication, §45) — are all *small, scalar-or-set* state. None of them models a
directory tree, and a tree is a different lattice.

Two traps frame the decision.

**The tree tempts a move-tree CRDT.** A filesystem has directories, and
directories can be moved, and a concurrent "move A into B" versus "move B into A"
is the textbook hard case a move-operation tree CRDT (Kleppmann et al.) exists to
solve atomically. It is real, it is correct, and it is a large net-new
correctness surface for a gain a two-person household almost never spends.

**A naïve register loses data.** The cheap alternative — one LWW register per
path, last writer wins — silently discards the losing edit. For reading position
"wherever I read to last" is exactly right; for a file it is data loss, and data
loss is the one thing a drive may not do.

## Decision

**A vault's catalog is a path-keyed map CRDT: a map from a normalised path to an
entry `{blob, size, content_mtime, versions[], writer, at}`, where `blob` is a
ciphertext blob id (ADR-0021). Directories are implicit — the shared prefixes of
live paths — not first-class nodes. A rename or move is a remove-at-old plus
add-at-new in one change. Two concurrent writes to one path both survive: the
one the total order does not pick is relocated to a "conflicted copy" path, so
the merge never discards bytes. Version history and trash are retention over the
immutable blobs the entries already reference, not a second store. The whole
catalog is encrypted under the space key and rides the existing state-sync
bridge as a new CRDT kind; a peer stores its changes as opaque ciphertext and
merges none of it.**

Five parts, one property: two replicas converge to a byte-identical filesystem
after any interleaving of offline edits, and no edit is ever silently lost.

### 🔴 The path is the key, and a directory is not a node

The map is keyed by the file's normalised path. A directory has no record of its
own; it exists exactly as long as some live path has it as a prefix. Therefore:

- **Rename / move** is `remove(old_path) + add(new_path, entry)` — two map
  operations carried in one change, ordered by the writer's causal key.
- **"Move a folder"** is the same operation applied to every live path under the
  prefix, emitted as one change. There is no folder object to move, create
  concurrently, or orphan, so the move-tree CRDT's hard case *does not exist in
  this model* — not "is handled", is unspellable.

This is the deliberate trade against a move-tree CRDT. What a move-tree buys is
atomic convergence of *concurrent directory moves*; what it costs is a bespoke
tree lattice with cycle-avoidance and tombstoned nodes, none of which heyarr has.
The path-keyed map reuses the OR-Set/LWW machinery the plane already ships and
trusts, and its worst case under a concurrent directory move is the same
conflicted-copy outcome every real sync tool produces. For a household drive that
is the right price.

### 🔴 A concurrent clash becomes a conflicted copy, deterministically

Two devices write different blobs to the same path with neither change's causal
key dominating the other (a genuine concurrent edit, not a happens-before). The
merge keeps **both**:

- The winner under the original path is chosen by the same total order
  `readingpos` already uses — a Lamport `at` counter, then a globally-unique
  `writer` tag (UUIDv7), then a final byte-level tie-break — a total order every
  replica computes identically.
- The loser is relocated to a derived path,
  `name (conflicted copy — <device label> — <at>).ext`, computed from fields
  every replica already holds, so every replica performs the *identical* rename
  and the map converges byte-for-byte (§43).

A write with a clear happens-before is **not** a conflict: the later write is the
current `blob` and the earlier one moves into `versions[]`. LWW-drop is rejected
here precisely because "keep the last write" for a file means "throw away
someone's edit," and the merge's contract is that it discards no bytes.

### 🔴 Version history and trash are retention over immutable blobs

Nothing new stores old versions, because ADR-0005 already made a blob immutable
and content-addressed and ADR-0018 already made deletion logical with GC
reclaiming bytes:

- A **prior version** of a file is its prior `blob`, retained in the entry's
  `versions[]`. A blob stays live while *any* entry references it — current,
  historical or trashed — so version history costs only delayed GC, never a copy.
- A **delete** is a tombstone carrying the last `blob`. The tombstone *is* the
  trash: the file is gone from the live tree, restorable while the reference
  stands.
- A **retention policy** — how many versions an entry keeps, how long a tombstone
  lives — is the only thing that drops a reference and lets GC reclaim the
  ciphertext. Its numbers are deliberately left open (see *Open questions*).

This keeps ADR-0021's and ADR-0036's line honest: version history protects
against a *bad edit or delete*; it does not protect against key loss or a
retention-expired delete propagating to every replica (§36 — replication is not
backup). An offline / immutable copy is still owed, and is not this record's job.

### 🔴 "Available offline" is a personal-state fact, never a control-plane want

The media library's `want`/placement machinery (§55, §37) is control-plane and
cannot see one byte of vault content, so it cannot be what marks a vault file for
offline use. A device pinning a file "available offline" writes a per-device
entry in the space (a device-scoped companion set), decrypted and acted on
client-side; the control plane never learns which files a device holds. Placement
of the *ciphertext blobs across sites* — the "replicated across both sites"
requirement — is a distinct decision (§37's all-peers default explicitly does not
carry to large vault objects, ADR-0021) and belongs to a forthcoming placement
record, not here.

### The kind, pinned

The catalog is a new CRDT `kind` beside `playlist`, `starred` and `readingpos`,
with its own snapshot type, encrypted under the space key (ADR-0049) and moved by
the existing `internal/personalstate/statesync` bridge as opaque changes. The
peer-side replication and protocol packages carry it exactly as they carry the
others — and, by the ADR-0049 depguard boundary, **cannot import the plaintext
drive model**, so a peer that peeks at a path or a filename does not compile.

## Consequences

- **The plane grows one kind, not one subsystem.** A new CRDT kind and snapshot
  join the three that exist; the store, bridge and peer surface carry it
  unchanged because they already move opaque encrypted changes. This is the whole
  reason the vault drive is "the personal-state plane growing large objects"
  (ADR-0021) rather than a new plane.
- **Both clients get conflict handling for free.** The desktop daemon
  (designated-folder full sync) and the mobile browser (on-demand + offline pins)
  are clients of this one CRDT. Their job is "diff the local tree against the
  decrypted map, encrypt-and-upload new blobs via the ADR-0021 path, emit map
  ops." Conflicted-copy relocation lives in the merge, so neither client
  reimplements it and neither can get it subtly different.
- **GC is now reference-counted over history.** A blob is reclaimable only when no
  current, historical or trashed entry references it. Under-counting a reference
  deletes a version a user could still restore — the sabotage target a test must
  make go red.
- **Path normalisation becomes a wire rule.** Two devices must derive the
  identical key for "the same path" or they will never converge and will conflict
  spuriously. Pin it: Unicode NFC, forward-slash separators, and a case
  discipline chosen once — a normalisation change is a new wire rule, not a
  reinterpretation.
- **Cross-user shared drives are explicitly out of scope.** This record is one
  user's vault wrapped for that user's own devices. A shared family drive
  (`KindFamily` / `KindShared`, §47) adds another user's writers to the same map
  and the authorisation to place them there — its own ADR, landed after this one,
  as ADR-0049 and this directory's conflict history advise.

## Alternatives rejected

- **A move-tree CRDT.** Covered above: it buys atomic concurrent directory moves
  at the cost of a bespoke tree lattice, for a case a household rarely hits. The
  path-keyed map degrades to a conflicted copy there, which is acceptable and
  familiar.
- **A flat collection, no folders.** Simplest, and not a drive — a person's
  files have structure and expect to keep it.
- **One LWW register per path.** Silently drops the losing edit; data loss is the
  one outcome a drive may not have.
- **Reusing the Work / Edition / Asset model.** That model is control-plane,
  plaintext-catalogued, and identifies content by an immutable digest. A drive
  file is mutable-at-a-path and must be invisible to the control plane. Wrong
  plane, wrong identity.

## What would make us revisit

- **Concurrent directory reorganisations that actually hurt.** If large,
  overlapping folder moves become a real source of conflicted copies, promote the
  map to a move-tree CRDT — a change of the catalog's lattice, not of the byte
  model beneath it.
- **Rich per-version metadata** (author, timestamp, a note) grows the entry; it
  stays one CRDT kind.
- **The shared-drive deliverable** triggers the §47 cross-user ADR.

## Open questions this record deliberately leaves

- **Vault blob frame size.** Fixed frames, no CDC (ADR-0021); the value trades
  range-read over-fetch against per-file overhead, and is pinned by the ADR-0021
  ingest implementation, not here.
- **Retention policy numbers.** How many versions an entry keeps and how long a
  tombstone survives before GC — the only knobs that let bytes be reclaimed.

## Relationship to existing records

- **ADR-0021** (encrypted vault content) owns the *bytes* — ciphertext blobs, the
  client-encrypts-before-upload ingest path, frame-aligned range reads. This
  record owns the *namespace* over them, the shape 0021 left open. 0021 moves to
  Accepted when vault behaviour ships; this rides with it.
- **ADR-0049** (space-key wrap) — the drive CRDT is encrypted under the space key
  exactly as `playlist`/`starred` are, and a peer stores its changes opaquely and
  merges none of them.
- **ADR-0051** (device gateway) — the catalog reaches clients through the device
  gateway, never the controller.
- **ADR-0005 / ADR-0018** — version history and trash are retention over
  immutable content-addressed blobs plus logical deletion and GC; this record
  adds no new byte store.
- **A forthcoming placement record** decides how a vault's ciphertext blobs are
  replicated across the two sites; ADR-0021 flags that §37's all-peers
  default does not carry to large vault objects.
- **§37, §43, §45** — the personal-state plane, convergence without a
  coordinator, and the existing CRDTs this one joins.
