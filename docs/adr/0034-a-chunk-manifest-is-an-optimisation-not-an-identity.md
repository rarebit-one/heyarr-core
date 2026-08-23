# 0034. A chunk manifest is an optimisation, and a blob is never addressed by its chunks

**Status:** Accepted
**Date:** 2026-08-23

## Context

Milestone 5 introduces a second content-addressed object into a system that has
had exactly one. §15 adds content-defined chunking and draws the manifest
hanging *below* the whole-file BLAKE3, then closes the section with the sentence
that decides everything downstream: *"Chunking does not replace the whole-Blob
hash."* ADR-0005 already said the same thing from the other side — chunk
manifests *"are **never** an identity"*.

Both statements are one line long, and one line is not enough once code exists
that has a manifest in hand and a blob it wants to name. The failure mode is
not that someone will disagree with §15; it is that a manifest is the more
*convenient* handle at exactly the moments identity is being decided. It is
already parsed, it is already keyed, and it is the thing the transfer path is
holding. Every content-addressed system that has made this mistake made it by
accident, in a lookup, not on purpose in a design document.

Two specific shapes have to be forbidden by name, because both are locally
reasonable:

- **Identity by chunk list** — using the manifest, or a digest over it, as the
  key a blob is stored, deduplicated or requested under.
- **Conflation by chunk list** — treating two blobs with the same chunk
  sequence as one blob, which is deduplication reasoning applied one level too
  high.

## Decision

**A manifest describes a blob. It is keyed by the blob's whole-object digest
and is never the key of anything.**

- A manifest is looked up *by* `blake3:<hex>`, the ADR-0005 identity of the
  blob it describes. There is no path — API, database, job payload, CAS lookup
  or transport frame — on which a blob is named by its chunk list, by a digest
  over its chunk list, or by any chunk of it.
- A manifest **may itself be content-addressed**, for its own integrity: a
  destination that is handed a manifest is entitled to verify it arrived
  intact. That digest names **the manifest**. It is not an alias for the blob,
  it does not appear in `blobs`, and nothing may resolve it to bytes.
- **Two blobs whose chunk sequences are identical are still two blobs** if
  their whole-object digests differ, and one blob if they do not. The chunk
  list is never consulted to answer *"is this the same blob"*. That question
  has had one answer since ADR-0005 and Milestone 5 does not add a second.
  Chunk-level deduplication is free to store those shared chunks once; that is
  a storage saving underneath identity, not a statement about it.
- **The destination still verifies the whole-object digest itself, on every
  path, including every path chunking introduces.** A chunked receive
  reassembles and hashes the assembled bytes against the digest it asked for,
  exactly as `PutExpecting` does today. Per-chunk verification does not
  substitute for it, and the reason is not caution: a set of individually valid
  chunks assembled in the wrong order is a set of valid chunks and the wrong
  file. Only the whole-object hash detects an ordering, duplication or omission
  fault, and invariant 1 is not satisfied by a proof about the pieces.
- **A manifest may be discarded at any time with no loss of correctness.** This
  is the operational test of the whole record: if deleting every manifest in
  the store breaks anything other than efficiency, the line has been crossed.
  Every blob remains addressable, servable, verifiable and replicable with no
  manifest anywhere — more slowly, and that is all. §16 already assumes this by
  making chunking lazy and noting that *"small Blobs may never require chunk
  manifests"*; a design in which manifests are load-bearing cannot also make
  them optional.

## Consequences

Manifests are regenerable state, so they can be rebuilt, evicted, versioned or
recomputed under a different chunker without a migration and without touching
blob identity. A future switch of chunking parameters is a cache invalidation,
not a data model change.

The whole-object hash is computed on every receive regardless of how the bytes
arrived, so chunking never buys back the hashing cost — it buys back the
*network*. That is the honest accounting, and ADR-0005's benchmark is the one
that bounds it: on disk-backed storage hashing is not the bottleneck anyway.

"Delete every manifest" is a legitimate, testable recovery action, and the
cheapest possible answer to a suspected chunker bug.

The pressure this record resists will come from the transport layer, where a
peer holding a manifest can address chunks and would like to address blobs the
same way. §22's BitTorrent transport is that pressure with a protocol behind
it, which is why it is the revisit trigger rather than a counter-example.

## Revisit if

**A transport arrives that addresses content by chunk rather than by object.**
§22 names BitTorrent, whose piece-hash tree *is* an identity within the swarm's
own namespace. That is not a contradiction of this record as long as it stays
inside the transport: the swarm's addressing is a transfer mechanism, the blob's
identity is the whole-object BLAKE3, and the destination still verifies the
latter before anything is published. It becomes a genuine revisit the day a
transport's chunk-level address is wanted as Heyarr's *stored* address — at
which point this record and ADR-0005 need rewriting together, not separately.
