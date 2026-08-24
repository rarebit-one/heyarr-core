# 0043. A piece transfer writes sparsely, and its record of what landed is a hint

**Status:** Proposed
**Date:** 2026-08-25

## Context

ADR-0042 chose Heyarr's own piece exchange over the peer surface. Wiring it to
bytes runs into a collision with ADR-0035, and the collision is structural
rather than incidental.

`cas.Partial` — the staging file a resumable transfer assembles into — is
**append-only**. Its interface is `Size`, `ReadAt`, `Append`, `Truncate`,
`Publish`, `Discard`, `Close`, and its doc defends the restriction on purpose:

> Truncate discards everything from n onwards. Shortening only: a partial never
> grows except by Append, because **a hole is bytes nothing wrote and a file of
> zeroes reads exactly like a file of received data**.

That is exactly right for what it was built for. ADR-0035's resumption keeps a
**verified prefix**, which is inherently sequential: a transfer knows how much
of the front it has proven and continues from there.

**Piece exchange is not sequential.** Pieces arrive out of order, from several
peers at once — that is the whole of §23, and it is why §24 wants a session with
several sources rather than a loop over one.

## Decision

**A piece transfer writes into its partial at arbitrary offsets, and records
which pieces have landed in an availability bitset stored beside the staging
file. That bitset is a HINT, never evidence.**

### Why the hole objection dissolves

The objection to sparse writes is precise: a hole reads as zeroes and is
indistinguishable from received data. With a bitset it is distinguishable —
**the bitset says the piece never landed**. The thing that made a hole dangerous
was the absence of a record, not the hole.

### Why a lying bitset cannot produce a wrong blob

This is the load-bearing part, and it is the same argument ADR-0034 makes for a
chunk manifest being an optimisation rather than an identity.

`Partial.Publish` re-reads the assembled file and hashes it **whole** against
the expected digest before linking anything into the blob tree. So a bitset that
is wrong — by a bug, a torn write, or a crash between the write and the record —
produces a digest mismatch and the transfer **fails closed**. It cannot produce
a wrong blob. The bitset is therefore free to be an optimisation: it saves
refetching, and it is never trusted.

That property is what makes this decision safe, and it is the first thing to
check if anyone proposes making `Publish` cheaper.

### What a resumed piece transfer is allowed to trust

ADR-0035 answered this for chunks — *"nothing it has not re-verified"* — and
this does not carve an exception into it.

A bitset found on disk is **a hint about where to resume**. Every piece it
claims is either re-verified against its piece hash before being counted, or
simply refetched. Both are cheap; neither trusts the file. The stance is
unchanged, and the bitset merely means a resumed transfer starts from a better
guess than zero.

### Where it lives

**Beside the staging file**, as `<digest>.pieces` under `tmp/`, not in the
catalogue.

Two reasons. `OpenPartial` already finds its own staging file from the digest
with nobody told where it is, and the record of what is in that file belongs to
the same place by the same logic. And ADR-0041 says a piece table is a transport
detail with a session's lifetime — putting it in the catalogue would make the
control plane know what a piece is, which is precisely the boundary that ADR
exists to hold.

It is reaped on the existing path: `ReapTemp` deletes abandoned `.part` files by
age, and a `.pieces` file is reaped with its partial. A reaped bitset costs a
refetch, which is what a reaped partial already costs.

## Consequences

- `cas.Partial` gains a sparse write. The append-only path stays exactly as it
  is for sequential resumption, and the doc keeps its warning — the restriction
  was right for that path and is not being relaxed there.
- **A partial can now be served from.** A peer holding some pieces can answer a
  ranged read for the pieces it has, which is the capability that turns a pull
  into a swarm (ADR-0042). What it must never do is serve a range it has not
  recorded as landed — that is the read side of the same bitset, and the one
  place where believing it wrongly sends bad bytes to somebody else rather than
  failing locally.
- A partial's `Size()` stops meaning "how much of the front is present". For a
  sparsely written file it is the high-water mark, and code that reads it as
  progress will overstate. Progress is `bitset.Count() / geometry.Count()`.

## What would make us revisit this

- **A store that is not a local directory.** `Partial` is an interface so that
  one can exist; a sparse write is natural on a file and may not be on an object
  store, where the answer is more likely to be "assemble in order" or "upload
  parts".
- **Publish becoming cheaper than a whole read.** The safety argument here rests
  entirely on the whole-object hash being computed on publication. If that ever
  becomes incremental or is skipped for a fast path, this decision has to be
  re-made with the bitset promoted from hint to evidence — which is a much
  stronger requirement than it currently meets.
