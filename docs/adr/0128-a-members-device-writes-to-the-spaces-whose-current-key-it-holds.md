# 0128. A member's device writes to the spaces whose current key it holds

**Status:** Accepted (2026-11-21, the owner's decision on #766, relayed by the director)
**Date:** 2026-11-21
**Refines:** [ADR-0067](0067-a-paired-device-enrols-itself-and-earns-only-the-read-floor.md) (answers its revisit trigger with a per-space rule)
**Builds on:** ADR-0065 (write by management authorisation), ADR-0096 (blind placement pins, provisional upload pins), ADR-0103 (key epochs), ADR-0104 (per-space grants for executors), ADR-0108 (membership)
**Refs:** #766, #764, #765

## Context

An enrolled device earns only the read floor (ADR-0067). To push a change,
upload a vault blob or pin it, a client needs `write`, which today means a
CLI-minted bearer token copied onto every device, or an operator authorising
the device for write on the whole node (ADR-0065). Both are wrong for a photo
app on a phone: the first is a per-node secret on every device, and the
second hands one app write to everything.

The owner decided the direction on #766: write derives from membership, per
space, and enrolment alone still grants nothing.

## Decision

**A Device credential whose device holds a copy of a space's current key, and
whose user the space's membership admits, may push changes to that space.
While it is such a member of at least one space, it may also upload vault
blobs, confirm its own user's upload pins, and record placement pins.**

- **The rule follows the key.** "Holds the current key" is a wrap for the
  device's encryption key at the space's current epoch (ADR-0103). A device a
  rotation left out is out. "Admitted" is ADR-0108's wrap gate: a family
  space admits every household user; a removed member is refused even while
  their stale copy remains; the owner and current members are admitted; a
  legacy space with no owner and no membership rows is gated by the key
  alone. An owned space that has no membership rows yet admits only its
  owner, so write is never wider than the wrap gate.
- **Per request, never on the identity.** The lift is a middleware on the
  routes that opt in (`RequireWriteOrMember`). A device's scopes stay
  `[read]`, so nothing else sees a wider device. An identity that already
  carries `write` passes as before. A lookup error refuses. The refusal is a
  403 with code `space_not_member`.
- **What opens.** `POST /spaces/{id}/changes` for that space, and only with
  `?key_epoch=N`: a member's push without it is `428 key_epoch_required`, and
  a stale one is the usual `409 change_key_epoch_mismatch`. Also
  `PUT /vault/blobs/{hash}`, `POST /vault/blobs/confirm` (held to blobs its own
  user uploaded, as an executor's is) and `POST /vault/placements`.
- **What stays closed.** Snapshots (they carry no key epoch yet), re-wrap,
  rotation, key deletion, compaction, replication, grants, membership,
  service recipients and space creation keep the scope they had. Removing a
  pin stays `write`, because unpinning is how a blob becomes collectable.
- **Vault content is "some space", not "this space".** A blob and a pin carry
  no space (ADR-0096, Invariant 6). Checking a named space would need the
  request to tell the server which space a blob belongs to, which is the
  mapping ADR-0096 exists to keep from the control plane. So the vault routes
  ask the question ADR-0104 asks for an executor: is this caller a writing
  member of some space? The concession is the same as ADR-0104's: a member of
  space A can place a blob it learned from space B. It can read none of B.
- **Derived, not recorded.** The decision on #766 mentioned recording the
  lift in the `space_grants` shape. We derive it on each request from wraps
  and membership instead. A recorded row would be a second source of truth
  that a removal or rotation would have to remember to revoke.
- **Clients can tell.** `GET /spaces/{id}/own-devices` gains `member_write`,
  true when the calling device may push to that space with its own
  credential. A node that predates this decision omits the field, so a
  client keeps using its write token there.

## Consequences

- A client app needs no bearer token to ingest into the spaces it is a member
  of. An optional bearer stays useful for admin actions only.
- `POST /enrol` still grants nothing. Membership, which is an owner's act on
  their own device (ADR-0108), is what lifts a device, one space at a time.
  ADR-0067's line holds: the unauthenticated route never becomes a
  write-granting one.
- A member's device can fill node storage with blobs while it is a member.
  This is the same exposure the write scope always had, bounded to devices
  someone chose to share a space with.

## What would make us revisit

- **A snapshot push gains a key epoch.** Then members may push snapshots too.
- **A member needs to wrap for its own new devices.** Re-wrap for the
  caller's own user's devices is the narrow next step (#766's plan), held back
  here to keep the first lift to the writes ingest needs.
- **Blob reads or writes need per-space confinement.** That means space-scoped
  blob ids, not a server-side index (ADR-0104's trigger).
