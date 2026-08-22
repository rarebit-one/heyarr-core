# 0030. Replication is a destination pull; the controller never carries bytes

**Status:** Accepted
**Date:** 2026-08-22

## Context

Spec §21 draws replication as `Peer A ─────► Peer B` and says the controller
*"authorizes and schedules the transfer"*. It never says which end opens the
connection, and the arrow reads as a push.

It is not a presentation detail. The two answers put the verification in
different places. §20's own pipeline is destination-centric — *"peer inventory →
compare manifests → missing Blob/chunk IDs → direct transfer → hash
verification"* — and §21 puts the verification obligation on the destination:
*"The destination verifies all content hashes independently."*

ADR-0013 already anticipated this and used the word that settles it: the blob
endpoint is *"deliberately the same endpoint that Milestone 4 replication reads
from"*.

## Decision

**The destination pulls.** It computes what it is missing, opens an mTLS
connection to a source the controller named, reads the ordinary blob endpoint,
and hashes the bytes as they arrive against the digest it expected.

The three roles are separate and stay separate:

- the **controller** schedules — it creates the job and names source and
  destination — and authorises, and never appears in a byte-carrying hop (§32);
- the **source** serves an ordinary blob read and makes no decision about what
  the destination needs;
- the **destination** decides what it is missing, pulls it, and verifies it.

**What "authorises" means**, since §21 uses the word and defines nothing: mTLS
membership under ADR-0012 *is* the authorisation for peer-to-peer blob reads. A
pinned peer in the membership record may read any blob the deployment holds. A
per-blob capability is deliberately not introduced — a Full Peer's desired blob
set is the complete canonical set (§19), so denying it individual blobs is
vacuous.

## Consequences

Authority over "what is missing" stays with the only party that can answer it
from its own disk. A push design has the source deciding what the destination
needs, which is the source making a claim about the destination's state — the
same class of mistake as trusting a source's claimed hash, one level up.

The source needs no replication-specific endpoint. It serves the blob endpoint
it already had, which is why ADR-0013 was written the way it was.

Because the controller is never in the data path, its availability is not
playback's availability or replication's. That decoupling is what makes §53's
degraded operation possible at all, and a proxy added for one awkward network
would quietly remove it.

## Revisit if

A peer class arrives whose desired blob set is a strict subset of the canonical
set — a Partial or Cache Peer (§6). Per-blob authorisation is vacuous for a Full
Peer and stops being vacuous for those, and that is the point at which the
authorisation half of this record needs rewriting.

A network where the destination cannot reach the source but the source can reach
the destination would put pressure on the pull direction. Note that it is not an
argument for proxying through the controller, which stays forbidden regardless.
