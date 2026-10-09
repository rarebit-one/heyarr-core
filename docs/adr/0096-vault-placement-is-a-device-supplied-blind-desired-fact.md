# 0096. Vault placement is an opaque, device-supplied desired fact the control plane executes blind

**Status:** Proposed
**Date:** 2026-09-15
**Amended:** 2026-10-09 — upload pins are provisional until a writer confirms them; see *Amendment: provisional upload pins (2026-10-09, #714)*.
**Milestone:** M9 — Encrypted personal state (the vault drive surface)

## Context

ADR-0095 gives a vault a filesystem; ADR-0021 gives it ciphertext bytes in the
CAS; ADR-0049 gives it keys no peer can unwrap. What none of them decides is how a
vault's ciphertext blobs come to exist **on both sites** — the plain "replicated
across both sites" requirement — when the control plane must never learn which
blobs belong to a vault (learning that is the membership leak the whole design
exists to deny, ADR-0021/§38).

The codebase has **two separate replication planes, and neither fits as-is.**

**CAS / blob replication is not blind, but only in its *planner*.** Desired
placement is not stored; it is *derived* every cycle. The target set is
`SELECT id FROM peers WHERE mode = 'full'` (ADR-0037's "all trusted Full Peers"),
and the desired blob set is
`SELECT DISTINCT blob_hash FROM assets WHERE blob_hash IS NOT NULL AND missing_since IS NULL`
— the canonical set is *whatever the catalog's `assets` rows name*. That derived
set feeds a pure diff, `Diff(peers, canonical, held) -> []Gap`, and each gap
becomes a `replicate_blob{BlobHash, DestinationPeerID}` job. The **executor is
already blind**: the job carries no source (ADR-0030 destination pull), the
destination ranks sources fresh and verifies by digest, and the source serves any
blob it holds with no decision about who needs it. The byte mover would move a
vault blob to a named peer today. The **planner is the leak**: the only thing that
makes a blob "desired" is an `assets` row that *names* it — and for a vault, an
`assets` row mapping space content to a `blob_hash` **is** the space→blob mapping
we may not record.

**Encrypted personal-state replication is blind, but cannot carry this.** It
pushes opaque per-space CRDT changes (capped ~1 MiB — "that is CAS sync's job") to
*every* Full Peer by fan-out. It has no per-space peer targeting and moves no CAS
blobs. Wrong vehicle.

So a vault's blobs sit between the two planes: content-plane bytes that must be
*placed* like blobs but *desired* without a catalog. The gap is precise: there is
no way for **"these blob hashes should be on these peers"** to enter the system as
a fact the control plane stores but did not compute and cannot attribute to a
space.

## Decision

**A vault's placement is a set of opaque `(blob_hash, target_peer)` desires,
submitted by a device — the only actor that holds the space key and therefore
knows which ciphertext blobs a space contains — stored by the control plane
carrying no space id, no path, no asset link, and unioned into the existing
desired-set computation so the existing blind executor replicates them. The
control plane learns that a blob should be on a peer; it never learns which space,
file, or user the blob belongs to. GC and the canonical-set derivation are taught
to honour these pins so a vault blob, which has no `assets` row by design, is
neither reclaimed nor forced to acquire a catalog row.**

Six parts, one invariant: the bytes converge across sites and the control plane
still cannot map a blob to a vault.

### 🔴 The desired set gains a second source, and the diff is already agnostic to it

`Diff(peers, canonical, held)` does not care where `canonical` came from. Today
one producer fills it (`assets`-derived). This record adds a second: a store of
`(blob_hash, peer_id)` placement pins, written by a device, that the planner
**unions** into the desired set before diffing. No `assets` row is created, so
`canonicalBlobs` never reads a space→blob mapping into existence. The transfer
plane — `replicate_blob`, source ranking, destination-pull digest verification,
the source serving any held blob — is reused **unchanged**. This is the design
working: the blind byte-mover already exists, and all a vault needed was a blind
way to say what to move.

### 🔴 What the control plane learns, stated plainly

A placement pin is `(blob_hash, target_peer)` and nothing else. From the set of
pins the control plane can see: *a number of ciphertext blobs exist, of such-and-
such sizes (the CAS knows blob sizes), and each should live on these peers.* It
**cannot** see: the space id, the path or filename, which blobs group into one
file or one vault, who owns them, or a single plaintext byte. This is the same
line ADR-0049 draws for wrapped-key membership — *structural, not content;
acknowledged, not hidden.* A deployment that finds even "N blobs of these sizes go
to these peers" too much is the trigger to wrap placement desires too (see *What
would make us revisit*); for a household it is the right price, and far less than
an `assets` row would leak.

### 🔴 The device is the author, an ADR-0048 grant is the authorisation

Only the device can name a space's blobs without leaking, so placement pins enter
through the device-facing personal-state surface (the gateway/API that already
accepts opaque changes and wrapped keys), authenticated as a device with write
scope (ADR-0065/0067). *Whether* a device may direct bytes at a given peer is an
ADR-0048 grant over that peer/site as a resource — the same orthogonality ADR-0049
drew: a grant lets you *place*, a wrapped key lets you *read*, and neither implies
the other. Authorisation is reused, not rebuilt.

### 🔴 Placement is explicit per vault, never the all-peers default

ADR-0037's "every Full Peer holds everything" default is set *because encrypted
personal state is typically small* — and ADR-0021 already flagged that it **does
not carry** to large vault media. So a vault blob is placed only where a pin says,
and "replicated across both sites" is the device pinning both. Silence means
"nowhere but where it was uploaded," not "everywhere."

### 🔴 GC and the canonical set must honour a pin, or they delete wanted bytes

A vault blob has **no `assets` row** — that is the whole point — so the
`assets`-derived canonical set excludes it and GC (ADR-0018, durability evidence)
would see an unreferenced blob and reclaim it. The fix is load-bearing: a
placement pin is a first-class reason a blob is *desired* and *safe*, unioned into
both the canonical set and GC's durability basis. **The sabotage target:** ignore
pins in GC and a family's vault blob is reclaimed out from under its replicas;
a test must go red.

### 🔴 A pin's lifecycle is the device's, tied to ADR-0095 retention

Blobs are immutable, so a key rotation (ADR-0049) or an edit (ADR-0095) produces
*new* ciphertext blobs; the device pins the new ones. When the drive CRDT's
retention finally drops a superseded or trashed blob (ADR-0095), the device drops
its pin, and only then may GC reclaim it across peers. Placement never resurrects
a blob the personal-state plane has let go, and never keeps one it still wants —
the pin follows the CRDT, client-driven.

## Consequences

- **One new opaque store and one new union; the transfer plane is untouched.** The
  pure `Diff` and every byte-moving path (`replicate_blob`, ranking, pull, verify,
  serve-any-held) work exactly as they do for catalogued content — the strongest
  evidence this is the right seam.
- **GC grows a second durability basis** (a pin, beside `verified_remote` /
  `sole_peer`). This is the one place a bug is silent and destructive; it is the
  demo's sabotage assertion.
- **The control plane's blind spot is preserved and quantified.** It gains a
  bounded structural fact (blob→peer, no space attribution) and loses no
  confidentiality it had; §38's list of what a server may not see is unchanged.
- **Upload and placement are two steps.** A device encrypt-uploads a vault blob to
  one peer's CAS (W1 / ADR-0021); placement then pulls it to the other site. The
  origin peer is just where the bytes first landed, not an authority.
- **Replication is still not backup** (§36, ADR-0021). Pinning to both sites is
  convergence, and a delete the device propagates removes the blob from both. A
  vault's real backup remains an offline/immutable copy, owed separately.

## Alternatives rejected

- **Give vault blobs `assets` rows so existing placement "just works."** That row
  is exactly the space→blob mapping we may not store; it hands the control plane
  membership. This is the leak ADR-0021's invariant exists to prevent.
- **Carry vault blobs on the encrypted-state fan-out plane.** It is push,
  all-peers, ~1 MiB, and moves no CAS blobs — it cannot target two named sites and
  cannot carry large media.
- **Inherit the all-peers default for vaults.** ADR-0021/§37: the default is
  premised on small state; vault media is not, so placement must be deliberate.
- **Let a peer place from the wrapped-key membership it can see.** A peer must not
  gain placement authority over content; keeping the executor a dumb, blind byte
  mover is what lets a vault be replicated to a site you trust less (ADR-0021).

## What would make us revisit

- **The blob→peer metadata is judged too much.** Then placement desires are
  themselves encrypted — a wrapped placement manifest expanded only by a
  key-holding planner — trading the clean "the planner is dumb" property for a
  smaller leak. Heavier; deferred until a deployment needs it.
- **Cross-user shared-space placement.** Who may place *another user's* space's
  blobs rides the §47 shared-spaces ADR, not this one.
- **Per-blob pins become a scaling problem.** A vault with millions of objects
  might want a per-space placement *policy* the device expresses once, expanded to
  pins by a key-holding agent, rather than a pin per blob. Argued then, not now.

## Relationship to existing records

- **ADR-0021** owns the bytes and already said the all-peers default does not
  carry to vaults; this record is the placement mechanism that replaces it.
- **ADR-0030 / ADR-0037** — the destination-pull executor is reused unchanged; the
  all-peers desired-set default is explicitly overridden for vault blobs.
- **ADR-0018** — GC must treat a placement pin as durability basis and desire, or
  it reclaims wanted vault bytes.
- **ADR-0048** — a grant authorises a device to direct placement at a peer;
  confidentiality (ADR-0049) stays orthogonal.
- **ADR-0049** — the "structural, not content" stance this record applies to
  blob→peer pins; and the rotation behaviour a pin's lifecycle tracks.
- **ADR-0095** — a pin's lifecycle follows the drive CRDT's version/trash
  retention; the device is the single author of both.
- **ADR-0065 / ADR-0067** — the device write scope the placement surface requires.
- **§37, §52** — the desired-state model and the materialised snapshot a peer
  already pulls against.

## Amendment: provisional upload pins, confirmed by the writer (2026-10-09, #714)

### Problem

The original record stated that a pin's lifecycle is the device's: the device
creates a pin when it uploads a blob and drops it when the drive CRDT no longer
references that blob. This model assumes the upload and the drive-change push
succeed together. In practice they are two separate network calls, and any failure
between them — a transport error, an access refusal, or (since #713) a
`change_key_epoch_mismatch` after a key rotation — leaves the blob pinned
permanently with no drive entry naming it. There is no unpin path for restricted
callers (`DELETE /vault/placements` refuses them by design, because unpinning is
how a blob becomes eligible for collection), so orphaned ciphertext accumulates as
an unbounded storage leak.

### Decision

**`PUT /vault/blobs/{hash}` records a provisional pin. The writer confirms it
after the drive change is accepted, making it permanent. GC may reclaim an
unconfirmed pin once its grace period expires.**

#### 🔴 Upload creates a provisional pin

The upload endpoint records two new fields per pin: `uploader` (the authenticated
principal) and `provisional_until` (a wall-clock expiry, 24 hours from upload
time). A pin in the store that carries no `provisional_until` is treated as
permanent, so no existing row is disturbed by the migration (see *Migration*
below).

#### 🔴 `POST /vault/blobs/confirm` makes pins permanent

After the drive change is accepted, the writer calls:

```
POST /vault/blobs/confirm
{"blobs": ["blake3:<hex>", …]}
```

For each listed blob the server clears the `provisional_until` field (the pin is
now permanent). Rules:

- **Executor scope.** A restricted executor may confirm only blobs whose stored
  `uploader` matches its own principal. An unrestricted (device-level) caller may
  confirm any blob it can name.
- **Idempotency.** Confirming a blob whose pin is already permanent is a no-op and
  returns success.
- **Unknown or unheld blob.** If the node does not hold the blob at all, the
  response includes a per-blob error; the remaining blobs in the request continue
  to be processed.

#### 🔴 GC reclaims expired provisional pins

A provisional pin whose `provisional_until` timestamp is in the past is treated by
GC as absent — the blob is subject to the existing grace window (ADR-0018) and
reclaimed if no other durability reason covers it (no permanent placement pin, no
`assets` row, no `verified_remote` / `sole_peer` status). This is the only change
to GC's durability basis.

#### 🔴 A permanently pinned blob is never reclaimed by provisional-pin expiry

If a blob is already permanently pinned — by any prior accepted change from any
writer — an upload by a second writer creates a provisional pin alongside it, but
the permanent pin takes precedence. When the provisional expires, GC sees the
permanent pin and leaves the blob. Confirming that second upload is still accepted
(idempotent). This covers the dedupe case: two writers uploading the same
ciphertext, which is only possible when both hold the same space key and seal
identical plaintext.

#### 🔴 Interaction with `change_key_epoch_mismatch`

The key-rotation race (#713): a writer uploads a manifest and content blob under
epoch N, receives `change_key_epoch_mismatch`, re-seals, and uploads two new blobs
under epoch N+1. Both uploads create provisional pins. After the epoch-N+1 change
is accepted, the writer confirms the epoch-N+1 blob ids. The epoch-N provisional
pins expire after 24 hours and GC reclaims those bytes without any manual
intervention.

#### 🔴 Migration: existing pins become permanent

A migration adds `provisional_until TIMESTAMP` and `uploader TEXT` to the
placement-pins store. Existing rows receive `provisional_until = NULL`, which the
server interprets as permanent. No expiry sweep runs against them.

#### 🔴 Compatibility

Clients that predate this amendment never call `confirm`; their upload pins will
expire after 24 hours, making their vault writes appear to have leaked. To handle
this during rollout, the server SHOULD gate provisional-pin semantics behind a
configuration flag, defaulting to the legacy permanent-pin behaviour until the
operator is satisfied that all active clients send `confirm`. The flag and its
effect MUST be documented in the configuration reference.

### Changed invariants

The sequence that produces a durable pin changes from `upload` to
`upload → change-accepted → confirm`. A pin existing in the store before this
amendment, or created while the compatibility flag is active, is permanent by
definition.

### Executor-rights note

This amendment grants restricted executors the right to call `confirm` for blobs
they uploaded themselves. It does not otherwise change the existing prohibition on
restricted executors dropping placement pins. ADR-0104 (a restricted principal
reaches only the spaces a device granted it) is the record of executor rights; this
confirm-only carve-out sits inside its per-space grant, so an executor can confirm
only blobs it uploaded into a space it was granted.
