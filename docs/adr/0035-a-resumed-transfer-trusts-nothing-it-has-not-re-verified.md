# 0035. A resumed transfer trusts nothing it has not re-verified itself

**Status:** Accepted
**Date:** 2026-08-23

## Context

Milestone 4 deliberately had no partial state, and said so in a package doc that
this milestone is about to invalidate:

> A failed transfer is retried whole rather than resumed. §84 puts resumable
> replication in Milestone 5, and the absence of resumption is what makes this
> handler idempotent rather than merely re-runnable: there is no partial state
> to be right about, because a receive that did not finish left nothing.

That was not a limitation to be lifted quietly. It was the reason the handler
satisfied invariant 9. Resumption reintroduces bytes that survive across
attempts, across process restarts and across crashes, so the question *what is
a resumed transfer allowed to believe about bytes it finds on its own disk*
has to be answered in writing before any code creates such bytes.

The tempting answer is the cheap one: record how many bytes arrived, reopen the
blob endpoint — which supports ranges, by ADR-0013 contract — and continue from
the offset. It is the shape every download manager has. It is also the shape
that quietly retires invariant 1, and the reason is mechanical rather than a
matter of taste, so it is set out in the decision.

## Decision

**A resumed transfer trusts nothing from the previous attempt except bytes it
re-verifies against a digest it holds independently.** The resume unit is
therefore a **chunk from a manifest the destination already has and has
verified**, never an opaque byte offset.

### Why a byte-offset resume is refused

A whole-object BLAKE3 cannot be continued across a process restart without
persisting hasher state. So an offset resume has exactly three possible
implementations, and all three fail:

1. **Persist the hasher state.** The destination then trusts a serialised
   intermediate that nothing verifies. Corrupt it — by a crash mid-write, a
   torn page, or a bug — and the resumed transfer produces a digest computed
   over a lie. It will usually mismatch and be caught; "usually" is not the bar
   invariant 1 sets, and a hasher-state file is a new unauthenticated input in a
   system whose entire premise is that there are none.
2. **Re-read the prefix and re-hash it.** This is correct. It also costs the
   full read of everything already received, which is the cost the resume
   existed to avoid. It saves network and spends disk, which is a real trade —
   but it is not resumption, it is a whole retry with a network optimisation,
   and it does not need persisted state or a new contract to exist.
3. **Skip the whole-object verification.** Forbidden outright by ADR-0005,
   ADR-0034 and invariant 1.

Per-chunk verification against a manifest is the only shape in which the
destination re-derives its trust from a **digest** rather than from a file it
left lying around. Each chunk it kept is re-hashed against the manifest entry
that names it; anything that does not match is discarded and refetched; and the
assembled result is still hashed whole before publication (ADR-0034). The
destination ends the resumed transfer having verified every byte it publishes,
having believed nothing it wrote earlier, and having read only what it kept.

### A blob with no manifest is not resumable

It is retried whole, exactly as Milestone 4 does today. That is not a gap to be
closed later — it is §16's lazy chunking doing its job, and it keeps the
existing handler's idempotence intact for precisely the blobs that never needed
chunking. A small blob's whole retry costs less than the manifest job would.

### Partial bytes are never addressable

- `replicas.state` keeps `pending` and `present`, and `present` is written
  **once**, after the destination has verified the assembled whole-object
  digest on its own disk. Nothing else may write it.
- No partial transfer is visible as a replica, as an inventory entry, or as a
  durability witness to garbage collection. The `present` row is a claim GC
  acts on (ADR-0018); a partial blob offered as one is how a deployment
  garbage-collects its last real copy.
- `replicas.bytes_present` is **progress telemetry, not an address**. It may be
  read by a human, a status endpoint or a scheduler deciding what to work on
  next. It may never be read as a resume offset, which is the same refusal as
  above wearing a column name.
- The blob endpoint's range support (ADR-0013) stays for the consumers it was
  built for — playback, remote probing, web-seeding. Replication resume does
  not become one of them.

### Unresumed partial state is reapable

Chunks staged for a transfer that is never resumed live in the store's private
staging area and are reaped on the existing staging/reap path. **Age is the
only thing that decides**: there is no reference count on a partial transfer,
no negotiation with the job queue, and no attempt to be clever about a transfer
that might come back. A reaped partial costs a refetch; a partial kept forever
costs disk in a system whose blobs are measured in tens of gigabytes.

## Consequences

Resumption is a property of chunked blobs, so it arrives exactly where it pays
— large blobs over slow or flaky links — and is absent where it would not have.
The two behaviours are one code path with a branch on "is there a manifest",
not two transfer implementations.

The handler stays idempotent under invariant 9 in a stronger sense than before:
a re-run may now find work already done, but it re-verifies that work rather
than trusting it, so the outcome of one run and of ten interrupted runs is
byte-identical.

The worst case is honest. A destination that keeps crashing re-verifies its
kept chunks each time and makes no progress on the corrupt one — visible,
diagnosable, and bounded by chunk size rather than by blob size.

Deleting the staging area at any moment is safe and costs only transfer work.
That is the same test ADR-0034 applies to manifests, and it should stay true.

## Revisit if

**A manifest-free resume becomes worth its cost.** The concrete trigger is a
change that removes reason (1) above — a hasher construction whose intermediate
state is itself authenticated, or a verified-streaming construction that lets a
destination prove a prefix without re-reading it. At that point option (2)'s
prefix re-read may also become cheap enough to simply do, in which case the
answer is "re-read and re-hash", not "trust the offset". Either way it is a new
record: the rule being relaxed is what a destination may believe, and that is
too close to invariant 1 to relax by editing this one.

Chunking parameters that make the manifest expensive relative to the blob would
push the other way — toward chunking more blobs, not toward offset resume.
