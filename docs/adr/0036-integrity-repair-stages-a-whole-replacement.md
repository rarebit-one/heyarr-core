# 0036. Integrity repair stages a whole replacement; a blob is never edited in place

**Status:** Accepted
**Date:** 2026-08-23

## Context

§15 lists *"repair of damaged replicas"* among the things chunking improves, and
the phrase that suggests itself is "replace the damaged chunk". Taken literally,
that is a write into a file that is currently named by a hash it no longer has —
and nothing in Heyarr has ever modified a blob's bytes:

- §14 makes blobs immutable; a modified file *is* a different blob (ADR-0005).
- Invariant 1 says the bytes are their digest, which means a file mid-repair
  answers to a name that is true of neither its old contents nor its new.
- ADR-0018 quarantines corruption rather than deleting it, because a blob that
  failed verification is evidence.
- The CAS has never had a mutating operation. `Put`, `PutExpecting` and `Link`
  all stage and publish; *"interrupting it must leave nothing addressable"*.

So the design question is not whether in-place repair is elegant. It is what is
addressable during the repair window, and what the store looks like if the
process dies inside it.

## Decision

**Repair reconstructs a whole replacement and publishes it atomically. The blob
under repair is never written to.**

The sequence:

1. **Reconstruct into the store's private staging area** — intact local chunks
   read from the damaged blob, plus replacement chunks pulled from a peer,
   each verified against the manifest entry that names it (ADR-0034,
   ADR-0035).
2. **Verify the assembled whole-object digest** against the blob's ADR-0005
   identity. A repair that cannot produce those exact bytes is a failed repair
   and publishes nothing.
3. **Quarantine the damaged original** (ADR-0018) — *before* publication.
4. **Publish** by the existing atomic path, the same one every other write to
   the CAS uses.

### What is addressable during the repair window

The original blob, in whatever state it is in, and nothing else.

There is no interval in which a partially repaired file answers to the blob's
digest. A reader that opens the blob mid-repair gets the damaged original —
which is correct: it is what this node actually holds, it will fail
verification the same way it did before, and a caller that needs good bytes has
a peer to read from (ADR-0030). The alternative — a reader briefly seeing a
half-repaired file under a name that certifies its contents — is the one
outcome content addressing exists to make impossible.

The staging file is never addressable, by the blob's digest or by any other
name, for the same reason a partial transfer is not (ADR-0035).

### What happens if the process dies inside the window

The store is in **exactly one of two states** — the pre-repair state or the
post-repair state — plus a reapable staging file. There is no third.

**Assert this by killing the process, not by reasoning about it.** A test that
argues the rename is atomic is a test of the author's confidence. The
acceptance evidence for this record is a test that interrupts a repair at each
step and reads the store back.

### Quarantine happens before publication, not after

The two orderings differ only in what a crash between them costs:

- quarantine, then publish → a crash loses the *repair* and leaves the damaged
  bytes safe in quarantine. The blob is missing from this node, which
  replication already knows how to fix, and the evidence survives.
- publish, then quarantine → a crash loses the *evidence*. The damaged bytes
  are gone, overwritten by the good ones, and the corruption that caused the
  repair can never be investigated.

The first is recoverable and the second is not, so the order is fixed.

### Why this is repair-by-chunk at all, given it writes a whole file

**The saving is in the network, not the disk.** §57's integrity reconciliation
is *"expected hashes vs verified bytes"*, and without chunking the only repair
available is refetching the entire blob from a peer — tens of gigabytes to fix
a few damaged megabytes. Chunking makes the repair *fetch* proportional to the
damage. The local read and the local write stay proportional to the blob, and
that is an acceptable price on a link that is slower than a disk, which is
every link this feature exists for.

## Consequences

Repair adds no new mutation primitive to the CAS. It composes staging,
whole-object verification, atomic publish and quarantine, all of which already
exist and are already tested — which is why this record forbids the shortcut
rather than describing a new mechanism.

Repair needs free space for a second copy of the blob while it runs. That is a
real operational cost on a nearly full volume and it is stated here rather than
discovered later; the mitigation is scheduling, not in-place writing.

A repair is indistinguishable, from the outside, from a fresh replication of
the same blob. The published bytes are the same bytes with the same name, and
every downstream consumer — replicas, inventory, GC, events — sees the
transition it already understands.

Every repair leaves a quarantined artefact. Quarantine grows and needs its own
retention story; that is ADR-0018's problem to extend, and it is the right
problem to have.

## Revisit if

**A filesystem primitive arrives that makes a verified in-place range write
atomic** — one where a torn write is impossible and the result is either the
old range or the new, observable as such after a crash. Reflink-based
copy-on-write already provides part of the machinery, and a future interface
that exposes an atomic range swap over it would make chunk-level in-place
repair defensible on the durability grounds this record refuses it on. It would
still have to answer the addressability question — what the blob's digest names
between the first and last range write — and if the answer is "bytes that do
not hash to it", the answer is still no.

A repair path that no longer needs the whole blob on disk — repairing directly
into a destination that is itself streaming to a peer — would also reopen the
free-space consequence above, without touching the atomicity decision.
