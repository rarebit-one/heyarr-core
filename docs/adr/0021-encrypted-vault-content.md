# 0021. Encrypted vault content

**Status:** Proposed
**Date:** 2026-08-19

## Context

ADR-0020 lets Heyarr show personal media without taking custody of it. Some
users will want the opposite: Heyarr holding their photos with the same
convergence and integrity guarantees it gives a film library, but without the
infrastructure — or whoever operates it — being able to read them.

Heyarr already has the mechanism. §39–§41 define EncryptedSpaces with keys
wrapped per device, where Full Peers store wrapped keys they cannot unwrap, and
§38 states that the server sees only opaque identifiers, causal metadata and
ciphertext.

The framing that makes this tractable: this is **not** adding encryption to the
content plane. It is the personal-state plane growing large objects.

## Decision

A **vault** is an EncryptedSpace whose members are content. Its bytes are
ordinary Blobs that happen to be ciphertext; its catalog is encrypted CRDT state
in the personal-state plane; the control plane holds nothing about it.

**Blobs are hashed over ciphertext, never plaintext.** Hashing the plaintext
would preserve cross-user deduplication, and it would also hand the server a
confirmation oracle: given a candidate file, hash it and look up whether this
user holds it. "Prove this person possesses this image" is precisely the
capability a vault exists to deny. Losing cross-user dedup is the correct
price — deduplicating personal photos across users *is* the leak.

**Content is encrypted in independently decryptable frames**, not as one stream,
so that a client can fetch and decrypt a byte range without the whole object.

**Clients encrypt before upload.** The server must never hold plaintext, so
vault ingest accepts pre-encrypted frames and an encrypted manifest. It is a
separate path from the plaintext ingest pipeline, not a variation of it.

## Consequences

**The storage fabric needs no changes whatsoever.** It moves opaque byte
sequences and verifies them against a hash; §21's independent verification at
the destination works on ciphertext without any key. Replication, placement,
integrity scanning and repair all work on a peer that cannot read a single byte
of what it is protecting. This is the whole reason the design is worth doing.

**Adding a peer does not widen the trust surface.** Keys go to devices (§40,
§41), not peers. A vault replicated to three sites is readable at none of them.
That makes it reasonable to add a peer somewhere you would not otherwise trust.

**Range serving stops being free.** ADR-0013 is load-bearing — Range powers
playback, remote probing (§29), replication and web-seeding (§27). Frame-aligned
encryption keeps it possible, but the client now translates logical ranges to
frames, over-fetches and trims. We lose `http.ServeContent` getting Range,
If-Range, 416 and multi-range right for free, and that code is easy to get
subtly wrong.

**Server-side transcoding and thumbnailing become impossible.** §68's playback
planner is DIRECT-only for vault content; the client must handle the format
natively. Thumbnails are generated client-side at ingest and stored as further
encrypted Blobs.

**The control plane cannot participate at all** — no title, no EXIF, no
duration, no server-side search. This is the design working rather than a gap:
§72 already establishes that controller-side MCP cannot decrypt user artifacts,
and §83 requires each feature to belong to one plane. A vault is content-plane
bytes plus personal-state-plane index, and no control plane.

**Key loss is total data loss.** With plaintext content a lost key is an
inconvenience; here every replica becomes permanently unreadable. Milestone 8's
"recovery" stops being a convenience and becomes the load-bearing part of this
feature. Do not ship vaults before key recovery is real.

**Replication is still not backup** (§36). Three converging replicas propagate a
delete to all three. A vault holding irreplaceable data wants a versioned or
offline copy exactly as much as plaintext content does — arguably more, since
nobody else has a copy to re-download.

**§37's default does not carry over.** It sets placement to all trusted Full
Peers *because encrypted personal state is typically small*. Vault media is not.
Placement must be a deliberate policy choice rather than an inherited default.

## What this is not

There are three tiers of "protected", and they are not close in cost:

| | Protects against | Costs |
|---|---|---|
| Plaintext CAS | nothing beyond OS permissions | — |
| Encryption at rest, server holds the key | stolen drives, offline access, a peer at a site trusted less | nothing — transcode, thumbnails and search keep working |
| **Vault (this ADR)** | Heyarr itself, and whoever operates it | everything above |

The middle tier is often what an operator actually wants, and it is available
outside Heyarr — filesystem-level encryption with a raw replication stream gives
an off-site peer that holds only ciphertext, with no loss of functionality.
Heyarr should say so plainly rather than pushing users to a tier whose costs
they have not weighed.

## Sequencing

Vaults depend on device keys (Milestone 8) and EncryptedSpaces (Milestone 9), so
they cannot land before those. Two decisions are needed earlier because they are
cheap now and expensive later:

- **Before Milestone 2:** probing and transcoding must not assume the server can
  always read plaintext.
- **Before Milestone 5:** content-defined chunking over ciphertext is pointless,
  since CDC boundaries do not survive encryption. Vault Blobs use fixed frames
  and skip CDC entirely.
