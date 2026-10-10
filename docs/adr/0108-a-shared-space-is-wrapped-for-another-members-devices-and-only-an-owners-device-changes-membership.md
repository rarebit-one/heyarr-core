# 0108. A shared space is wrapped for another member's devices, and only an owner's device changes membership

**Status:** Proposed
**Date:** 2026-10-10
**Milestone:** M9 — Encrypted personal state
**Builds on:** [ADR-0049](0049-a-space-key-is-wrapped-for-a-device-encryption-key-and-a-peer-cannot-unwrap-it.md), [ADR-0095](0095-a-vault-is-a-filesystem-as-a-path-keyed-map-crdt.md), [ADR-0103](0103-a-space-key-has-epochs-and-a-rotation-seals-the-previous-key-under-the-next.md), [ADR-0104](0104-a-restricted-principal-reaches-only-the-spaces-a-device-granted-it.md)

## Context

This is the §47 decision that ADR-0049 and ADR-0095 explicitly deferred.
ADR-0049's "What would make us revisit" names it: "Cross-user shared spaces —
its own ADR when the shared-spaces deliverable is built." ADR-0095's
consequences note: "Cross-user shared drives are explicitly out of scope; its
own ADR, landed after this one."

§47 says shared playlists or group artifacts use their own space keys, wrapped
for each participant. The model already exists in code: `internal/personalstate/spaces`
defines `KindPersonal`, `KindFamily`, `KindShared` and `KindResearch` (§39).
The kinds exist; their membership semantics do not.

Three open questions frame the decision.

**Who owns a space and who may change its membership.** ADR-0104 records a
space owner as the user who created it from a device, and its owner-less
(pre-existing household space) rule: any authorised device may grant there. A
shared space needs a matching rule, and it must be enforceable across two sites
that may be partitioned at the moment membership changes.

**Whether sharing is per-item or per-space.** Per-item ACLs let a member share
one file from a space while keeping the rest private. Per-space sharing wraps
the whole space key. ADR-0049's scheme wraps `K_space` per device; adding
per-item keys would require a separate key per item and a second-level wrap, a
new primitive not yet present. ADR-0097 seals every frame under the space key —
an item-level key would require re-sealing every frame on every membership
change, or building a per-item key hierarchy on top of an unchanged frame
codec. Either direction is a significant new surface.

**Whether membership is a node-local or replicated fact.** ADR-0104's executor
grants are node-local: an executor talks to exactly one node, and that node's
grant table is the authority. A shared space between two household members on
two sites cannot be node-local: a membership change on Site A must eventually
make content readable at Site B. Wrapped keys already replicate with the space
(ADR-0049, ADR-0101); a membership record that does not replicate would leave
Site B refusing fetches until it is told separately.

## Decision

**The space is the unit of sharing. A shared space is wrapped for each enrolled
device encryption key of every member; membership changes are replicated facts.
Only the space owner's enrolled device may add or remove a member from a
`personal` or `shared` space; a `family` space has no single owner and any
authorised device may change it. Members may not re-share. Sharing content
means placing it in a shared space; there are no per-item keys or ACLs.**

### The space kinds now carry explicit membership semantics

| Kind | Members | Who changes membership |
|---|---|---|
| `personal` | One member's enrolled devices + the recovery key | The space owner's enrolled device |
| `family` | Every enrolled device of every household member | Any authorised device (ADR-0104's owner-less rule) |
| `shared` | A named subset of household members | The space owner's enrolled device |
| `research` | One member's enrolled devices (personal use) | The space owner's enrolled device |

An album or folder lives inside a space. Sharing it means placing it in a
`family` or `shared` space. Membership is at the space level, not the item
level.

### Adding a member wraps the current space key for each of their enrolled devices

`POST /spaces/{id}/members` names the member user. The server resolves their
enrolled device encryption keys from `device_identities`, and the calling
device wraps the current space key at the current epoch (ADR-0103) for each of
those keys in one transaction. Enrol-before-wrap (ADR-0049) already refuses a
wrap for any key not pinned in `device_identities`, so the only valid targets
are enrolled device keys, with no exception. Adding a member whose devices are
not yet enrolled is refused; they enrol first.

The calling device must hold the space key to perform the wrap. It reads the
key by opening its own wrap from the key history, wraps for the new member's
keys, and submits the wraps with the membership change. The server checks:

- the caller holds an enrolled device credential with write scope (ADR-0065);
- for `personal` and `shared` spaces, the caller is the recorded space owner;
- for `family` spaces, the caller is any authorised device (no owner check);
- the epoch in the submitted wraps matches the current epoch (ADR-0103's
  compare-and-swap — a concurrent rotation gets a 409 and the caller re-opens
  the space).

Each grant and revoke is recorded as an event (Invariant 7).

### Removing a member rotates — they keep what they already fetched

Removal is a space rotation (ADR-0103) that excludes the removed member's
device keys. The rotation produces a new epoch; the removed member's copies are
deleted from the node; the new epoch's wraps are pushed to all remaining
members' devices. The removed member loses access to future content
immediately; they retain every change they already downloaded under the
previous key. This is forward secrecy of future content, not erasure of the
past. State this plainly; there is no mechanism to make it otherwise without
re-encrypting every historical change.

Nothing is re-encrypted retroactively on removal. The cost ADR-0021 accepted
for a per-device key model ("key loss is total data loss / replication is not
backup") applies symmetrically here.

#698 (rotation strands vault files, blocked by key-epoch history not yet
implemented in full) must be resolved before removal is enabled for spaces that
contain vault blobs. The `POST /spaces/{id}/members/{id}` DELETE endpoint
records the removal and triggers the rotation call; the rotation is guarded by
the same #700 flag that blocks vault rotation today, lifted when #698 ships.

### Membership for a shared space is a replicated fact

Unlike ADR-0104's executor grants, which are node-local (an executor talks to
exactly one node), shared-space membership must replicate. A household on two
sites must agree on who is a member or one site will refuse fetches the other
has begun serving.

The wrapped keys already replicate with the space (ADR-0101, ADR-0049). The
membership record — which users and which device key fingerprints hold wraps —
piggybacks on that replication. A peer receiving a rotation that excludes a
device key drops that wrap. A peer that has not yet received the rotation still
serves old-key content to the removed member; once the rotation replicates,
the member's access closes on that peer too. Replication latency bounds the
window; there is no mechanism to close the window before replication completes
without a strongly consistent membership service this record does not introduce.

### Members may not re-share

A member may not wrap the space key for a third party's devices. The server
enforces this: `POST /spaces/{id}/members` checks the owner rule (above) and
refuses any caller who is not the recorded owner or, for `family` spaces, an
authorised device. A member who receives a space key client-side can of course
copy it; that is the limit of what the server can prevent.

### Bytes crossing a key boundary are re-sealed under the destination space key

Moving an item from a `personal` space into a `shared` space means the item's
ciphertext — sealed under the personal space key (ADR-0097) — cannot be read
by the shared space's members. The client re-seals each frame under the shared
space key and uploads new ciphertext blobs; the drive entry in the shared space
names the new manifest blob id. The original personal space's blobs become
reclaimable once its drive CRDT no longer references them.

Ciphertext may therefore exist twice if the item is "linked" from both spaces
by re-sealing — the price ADR-0021 already accepted to deny a confirmation
oracle (hashing plaintext would reveal identity across users; two users sealing
the same plaintext produce different ciphertext under different keys). Benefits:
no per-item keys, removal is one rotation, each space's blobs have their own
placement pins (ADR-0096) and their own GC lifecycle (ADR-0018).

## Consequences

- #698 blocks member removal from vault spaces until rotation no longer strands
  vault files. The membership route ships; the removal path is guarded by the
  same flag as vault rotation.
- A membership change is an event (Invariant 7). The event names the device
  that acted and the user whose membership changed; it does not name which
  device keys were added or removed, because that is structural, not content.
- The peer sees which users are members of a space (the user-membership record
  replicates), and which device key fingerprints hold wraps (ADR-0049 already
  acknowledged this as structural, not content). It still sees no plaintext.
- Moving an item between spaces requires the client to re-seal every frame. For
  a large vault file this is a full re-upload under the destination key. The
  cost is accepted; a reference-sharing scheme that avoids it would require
  per-item keys, which this decision rejects.
- A `family` space's "any authorised device may grant" rule means a family
  member can add another member without the original creator's involvement.
  This matches how a household shared drive works in practice; an owner-only
  rule for a `family` space would require a tiebreak for who owns it, and that
  tiebreak is not specified.

## What would make us revisit this

- An operator wants per-item access control inside a shared space. That needs
  a per-item key layer on top of ADR-0097's frame codec — a new primitive, not
  an extension of this record.
- Re-delegation by members is wanted. The server cannot prevent it at the
  protocol level; an explicit policy, enforced client-side, is the only lever.
- Retroactive revocation — a removed member must lose access to content they
  already downloaded. That requires re-encryption of every historical change,
  and the cost is unbounded for a large vault.
- The replication-latency window on removal is judged too long. Closing it
  requires a membership service with stronger consistency than eventual
  replication provides.

## Alternatives rejected

- **Per-item keys and ACLs.** Requires a second key hierarchy on top of
  ADR-0097's frame codec. Removal triggers a per-frame re-seal of every item
  the removed member could see, or the revoked key must remain valid for
  historical frames. Either direction adds a new primitive and a new attack
  surface for a case ADR-0095 already accepted a whole-space rotation covers.
- **Re-delegation by members.** Allows a member to widen a shared space without
  the owner's involvement. Membership changes are owner-gated by design so the
  sharing surface remains auditable from a single root.
- **Retroactive re-encryption on removal.** ADR-0049 already committed to
  forward secrecy of future content only, not erasure of the past. Retroactive
  re-encryption is unbounded in cost for a large vault and would block the
  rotation API for an indeterminate time.
- **Reference-sharing across spaces (no re-seal on move).** A file in two
  spaces would share a blob id; the blob's ciphertext would be sealed under
  one key and unreadable under the other, or a plaintext blob hash would be
  needed to de-duplicate — which ADR-0021 rules out as a confirmation oracle.
  Per-item keys would be required to make reference-sharing work without leaking
  cross-space identity, returning to the alternative rejected above.
