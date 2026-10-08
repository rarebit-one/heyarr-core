# 0103. A space key has epochs, and a rotation seals the previous key under the next

**Status:** Proposed
**Date:** 2026-10-08
**Milestone:** M9 — Encrypted personal state
**Amends:** [ADR-0049](0049-a-space-key-is-wrapped-for-a-device-encryption-key-and-a-peer-cannot-unwrap-it.md) (one key per space; revocation re-encrypts forward)

## Context

ADR-0049 gives each space exactly one key, and `heyarr space rotate` replaces it.
The client then discards the old key. Rotation re-encrypts a playlist forward
through a snapshot and compaction, which works only because a playlist can be
re-materialised. A vault drive cannot be re-materialised: its file frames and
manifests are sealed directly under the space key. So rotating a vault space
makes every pre-rotation file unreadable (#698). #700 blocks rotation for
everything but playlists until this is fixed. Review of #700 found a second
hole: a writer racing a rotation can still seal a change under the old key, and
that change is lost too.

Replication adds a third. Wrapped keys replicate last-write-wins and deletes do
not replicate. A peer that missed a rotation can therefore push back an old
wrap, including the copy of a revoked device.

## Decision

**A space key has an epoch. Epoch 0 is the key the space was created with.
Rotating to epoch N stores one opaque history row: key_{N-1} sealed under key_N
by the client. Rotation is a pure re-key, with no snapshot and no compaction.
It is a compare-and-swap on the epoch. Every wrapped copy carries the epoch it
seals, and the peer refuses a copy that is not at the current epoch.**

- **The row's format.** `sealed_prev` is
  `encryption.SealSpaceKey(key_N, key_{N-1})` from void-which-binds-go ≥ v0.26.0,
  opened with `OpenSpaceKey`; its key-chain test vector pins the bytes, so every
  client (the mobile one included) reads a chain any other wrote.
- **Reading the past.** A current recipient unwraps key_N and opens each history
  row in turn back to epoch 0. Content sealed under any earlier key stays
  readable, including a change a racing writer sealed under the previous key.
  Nothing is re-encrypted, so rotating a large vault costs one row and one wrap
  per recipient.
- **Revocation stays forward-only.** The rotating client wraps key_N only for the
  recipients that keep access. The peer deletes every wrap below N in the same
  transaction, so the revoked device never receives key_N. It loses nothing it
  did not already have: it held every earlier key, and the history row gives it
  nothing new. ADR-0049 already promised forward secrecy of future content, not
  erasure of the past. The recovery key must be re-wrapped, because the chain
  seals backwards only: a recovery key left on the old key never reaches the
  new one. The peer refuses a rotation that leaves out a recovery key that
  holds a copy of the current key (ADR-0022). The device store tells the peer
  which recipients are recovery keys. Recovery then wraps a recovered key at the
  current epoch only after the key opens the newest history row.
- **The peer stays blind (Invariant 6).** A history row is ciphertext under a
  symmetric space key the peer never holds. The peer enforces only structure:
  one row per (space, epoch), and a current epoch derived as `MAX(epoch)` with
  no stored column that could drift.
- **Rotation cannot fork.** `POST /spaces/{id}/rotate` (`admin`) names the epoch
  it rotated from. A second rotation from the same epoch gets a 409 and must
  re-open the space.
- **The compare-and-swap covers the recipient set too** ([#703](https://github.com/rarebit-one/heyarr-core/issues/703)).
  Adding or removing a recipient does not move the epoch, so the rotation also
  names the recipients it revokes. Its wraps plus that `revoke` list must be
  exactly the recipients holding a copy of the current key. Otherwise it gets a
  409 (`rotation_recipients_changed`): a recipient added since would silently
  lose access, and one removed since would be handed the new key. `revoke` is
  required, so a recipient is never dropped by omission.
- **Replication cannot regress.** A peer pushes history rows first, in ascending
  order, and then its wraps tagged with their epoch. A row that becomes the
  target's newest epoch drops the older wraps there, which is how revocation
  replicates without a delete message. A wrap below the target's epoch is
  answered "superseded" and skipped, so the stale copy is never resurrected. A
  different row for a held epoch is a fork. The peer refuses it rather than
  picking a winner.
- **No snapshot or compaction.** This matches ADR-0101: until #681 makes
  snapshots trustworthy, nothing a rotation does may depend on one.

This record covers the server and the wire. The client keyring and the
re-keying `space rotate` follow in a separate change; see Rollout for when the
#700 guard lifted.

## Rollout

The client keyring and the re-keying `space rotate` landed in #702, with the
#700 guard still in place: until then only playlist spaces rotated, and a
rotation also carried the playlist forward as a snapshot under the new key and
compacted the old log, for clients that held only the newest key. Both were
lifted once the mobile client shipped its keyring
([heyarr-kmp#118](https://github.com/rarebit-one/heyarr-kmp/issues/118)). Every
kind of space now rotates as the pure re-key above, and `device revoke` re-keys
every space the revoked device could read.

## Consequences

- A device that wants old content needs the whole chain. The chain grows by one
  small row per rotation, which is negligible at human rotation rates.
- A device that adds a recipient must wrap the current key at the current epoch.
  A stale device gets a 409 and must re-open the space, so it cannot hand out a
  superseded key.
- Each epoch row is pushed on every reconcile, and the target treats a repeat
  as a no-op. A peer that predates epochs rejects the history route, so a
  rotated space defers on that peer until it upgrades. Epoch-0 pushes are
  byte-identical to the old wire format.

## What would make us revisit

- A need for **retroactive** revocation, meaning a revoked device must lose
  access to past content it never downloaded. That needs re-encryption, and the
  re-materialise cost this record avoids.
- Rotation rates high enough that unrolling the chain costs noticeably at open.
  The fix would be a periodic re-wrap of a checkpoint key, not a change here.
- Snapshots becoming trusted (#681). A rotation could then optionally compact
  under the new key and let the chain be truncated.
