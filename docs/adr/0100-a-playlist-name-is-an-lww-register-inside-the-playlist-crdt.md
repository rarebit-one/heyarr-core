# 0100. A playlist's name is an LWW register inside the playlist CRDT, and unknown ops are ignored first

**Status:** Proposed
**Date:** 2026-09-28
**Milestone:** M9 — Encrypted personal state (display metadata over the device gateway)

## Context

A space carries no name, on purpose. The peer sees a space's `kind` and nothing
else (§38, §39), and the demo asserts it. So when the device gateway (ADR-0051)
answers `getPlaylists`, it has nothing to show but `"Playlist " + <space id>`
(`internal/device/gateway/personal.go`). The first stock app pointed at the
gateway found this (#605): the invariant held, and the playlists were
indistinguishable 36-character ids.

The name is personal state, so it cannot go on the space record. It has to live
inside the ciphertext. What remains to decide is **where** inside the ciphertext,
and **how an existing reader copes with a change shape it has never seen**.
The second question is where the risk is:

- Go's `crdt.State.applyOne` switches on `Op` and silently ignores an unknown
  one. That is safe.
- The shipped Android client (`heyarr-kmp`,
  `androidApp/.../personalstate/Playlist.kt`) decodes with
  `PlaylistOp.of(wire) = if (wire == REMOVE.wire) REMOVE else ADD`. **Any unknown
  op becomes an ADD** of whatever `ItemID` it carries (empty, for a name
  change). That replica would grow a phantom item that no Go replica has, and
  the two would diverge. §43 does not allow that.

## Decision

**A playlist's display name is a last-writer-wins register inside the playlist
CRDT, carried as a third op (`OpName`) with the same `(At, Writer)` total order
that `readingpos` uses, and included in the playlist snapshot as an optional
field. Before any client writes a name, every playlist reader must ignore an
op it does not know. The Android decoder is fixed and shipped first.**

1. **Tolerance first.** Change `PlaylistOp.of` (and any other Kotlin CRDT
   decoder with the same fallback, such as `Starred.kt`) so an unknown op is
   skipped, never coerced to ADD. Add a parity vector with an unknown op to both
   the Go and Kotlin suites. This change is useful on its own, and it is the
   precondition for everything else.
2. **The register.** `OpName` carries `{Name, At, Writer}`. The state keeps the
   greatest `(At, Writer)` name, with the name string as the final tie-break, as
   `readingpos` does. It is a max-register, so it joins commutatively,
   associatively and idempotently beside the OR-Set. A concurrent rename loses
   only a label, never an item.
3. **Snapshot.** `stateSnapshot` gains an optional `name` object. An absent
   `name` means "never named". Snapshots stay deterministic.
4. **Fallback.** `SpaceLibrary` reads the register and falls back to today's
   synthesised `Playlist <id>` when it is empty, so every existing space keeps
   working unchanged.
5. **Surface.** A sibling command to `heyarr space put` sets the name. The
   gateway reports it. `songCount` stays derived from the materialised items and
   is never stored.

**Starred and history get no name.** Each is a per-user singleton, so the
gateway can label it with a fixed string. #605 asked for one decision covering
all three. This is that decision: only a kind a user can create many of needs a
name.

## Consequences

- The rollout has an order: first the tolerant decoders on every client that
  folds playlists, then the writers. Writing a name before the Android fix
  reaches devices would make those devices diverge.
- A client that predates the snapshot field and **pushes a snapshot** would
  drop the name from it, and compaction could then lose the name for good. Today
  only the Go CLI and the gateway snapshot playlists. Before any other client
  starts snapshotting a playlist, it must understand `name`.
- No change to the peer, the store, or the envelope. The peer still stores
  opaque changes and learns nothing new (invariant 6).

## Alternatives rejected

- **The name on the space record.** It hands personal state to the controller.
  This is the thing the plane exists to prevent.
- **A per-user "directory" space mapping space id to name.** It keeps the
  playlist CRDT untouched, but a playlist shared with another user (§47) would
  not carry its name to them. It also adds a cross-space consistency problem for
  a label.
- **Disguising the name change as a no-op REMOVE (`Op: 1`, nothing observed)**,
  so old decoders skip it. It is back-compatible today, but it builds the wire
  format on a trick every future reader has to know about. The tolerant-decoder
  step fixes the real bug instead.

## What would make us revisit

- A second piece of per-playlist display metadata (a cover or a description). It
  should join the same register as a struct, not become a new op per field.
- Shared multi-user playlists (§47), if one user's rename should not overwrite
  another's view.

## Relationship to existing records

ADR-0051 (the gateway reads materialised state), ADR-0049 (encrypted under the
space key), ADR-0095 (the drive's `(At, Writer)` order this reuses), §38, §43,
§72, §73. Tracks #605.
