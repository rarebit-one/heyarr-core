# 0041. A TransferSession is local to a peer, and pieces are not chunks

**Status:** Proposed
**Date:** 2026-08-24

## Context

Milestone 6 makes acquisition and replication stop being sequential: rather than
`Internet → peer A` then `peer A → peer B`, both peers join one swarm, exchange
completed pieces with each other while pulling from external sources, and both
arrive as complete replicas (§23, §24).

§24 names the shared abstraction — `TransferSession`, with a target Blob,
participants, available sources, transport, priority and urgency — and stops
there. Three decisions have to be made before a transport is built rather than
during, because each is cheap now and expensive to retrofit.

Two of them were raised on the epic (#25) and one is a hazard the milestone
brief flagged:

- **Does a peer that can be reached, but cannot reach back, participate?** M4
  found a real pairing where one host reaches the other and the reverse fails
  completely (#186). A swarm is a stronger requirement than a fetch.
- **How does a BitTorrent piece size relate to a FastCDC chunk boundary?** They
  are both "pieces of a file", which is exactly how a subtle dedup loss gets
  shipped.
- **What owns a session, when ADR-0038 says there is no controller?**

## Decision

### 1. A TransferSession is LOCAL to one peer. There is no shared session.

Each peer that wants a Blob runs its own session for it. Sessions discover one
another through authenticated membership (§26) and cooperate, but no session is
authoritative for another and none is a coordinator.

This follows from ADR-0038 rather than being a fresh choice: there is no node
whose loss is special, so there is nothing for a shared session object to live
on. A session that had to be created somewhere would reintroduce a controller by
the back door — and it would be a controller in the one path where a peer that
cannot reach it is supposed to be having an ordinary day.

The consequence worth stating plainly: **two peers acquiring the same Blob are
two sessions that found each other**, not one session with two members. What
they share is the target digest, which is the only identity there is
(invariant 1).

### 2. A peer participates in whatever DIRECTION works, and never refuses.

Participation is decided per pair and per direction: if A can dial B, A dials B;
if only B can dial A, B dials A, and A is a seed-only member of that pair from
its own point of view. Neither peer refuses the session for it, and neither
reports it as a fault — ADR-0037 already says one-way reachability is reported,
not refused, and ADR-0038 already says an unreachable peer is ordinary.

A pair where **neither** direction works simply does not exchange with each
other. Both continue pulling from external sources and from any third peer they
can reach. That is a smaller swarm, not a failure, and nothing in the session
may treat it as one.

The rule this gives the implementation: **a session makes progress with whoever
it has.** It must never wait for a quorum of participants, never block on an
unreachable one, and never let the set of reachable peers change its completion
criteria. Completion is "the target digest verifies locally", full stop.

### 3. Pieces and chunks are INDEPENDENT partitions. Neither derives from the other.

A FastCDC chunk is content-defined and variable length (256 KiB / 1 MiB / 4 MiB
at the shipped parameters). Its boundaries move with content, which is precisely
what makes them survive an insertion — and that is why two peers that chunk
differently share nothing, and why the boundaries are pinned by golden fixtures
across Linux, macOS and Windows.

A BitTorrent piece is fixed length and aligned from offset zero. The protocol
requires uniform piece length; there is no version of this where piece
boundaries follow content.

So they cannot be unified, and the two ways of trying both lose something real:

- Making chunks fixed-size to match pieces **destroys dedup and resumability**.
  Content-defined boundaries are the entire mechanism; a fixed grid shifts
  wholesale when a byte is inserted. This is the loss the milestone brief warned
  would be subtle, and it would present as "replication just moves more bytes
  than it used to", with nothing red.
- Making pieces follow chunks is not expressible in the protocol.

They are therefore **two independent partitions of the same bytes, kept for
different purposes**:

| | unit | why it exists | lifetime |
|---|---|---|---|
| chunk | content-defined | dedup, reuse, resume, repair | durable (manifest) |
| piece | fixed, aligned | swarm exchange and its verification | the session only |

**A piece table is a transport detail and is never an identity.** It is not
persisted as one, it does not appear in a manifest, and it never addresses
anything — ADR-0034 already says a chunk manifest is an optimisation and not an
identity, and a piece table is a weaker thing than that.

### 4. Verification is two-level, and the outer level is unchanged.

Because a piece is not aligned to a chunk, **a received piece cannot be checked
against a chunk digest**. Verification is therefore:

- **during transfer** — each piece against the swarm's own piece hashes, which
  is what makes an exchange safe against a lying peer;
- **on completion** — the whole object against its BLAKE3 digest, which is
  invariant 1 and is not weakened, delegated or skipped. A destination verifies
  bytes itself and never trusts a claimed hash (§21, ADR-0005).

The chunk manifest keeps its M5 role on either side of the exchange and takes no
part in it: **reuse planning before** a session starts (what does this peer
already hold, so what need not be fetched at all — ADR-0035) and **repair
after**. That is a narrowing of what a manifest is for, and it is deliberate.

## Consequences

- `TransferSession` can be built against replication and acquisition as peers of
  each other, because neither owns it and both hand it the same three things: a
  target digest, a set of sources, and an urgency.
- The internal transport stays separable from the external acquisition client
  (§25) — Transmission grabs releases from the Internet and has no part in
  inter-peer exchange.
- Web-seeding (§27, ADR-0013) fits without a new surface: the ordinary blob
  endpoint already serves byte ranges, and a fixed piece is a byte range. It
  needs no piece awareness on the serving side at all.
- A session cannot report "blocked on an unreachable peer", because that state
  does not exist. It reports what it has and what it still needs.

## What would make us revisit this

- **A programmable BitTorrent engine that permits non-uniform pieces.** Decision
  3's arithmetic changes if piece boundaries can follow content, and the two
  partitions could then collapse into one.
- **Measurement showing the double partition costs more than dedup saves.**
  Nothing here is measured yet; the dedup benefit is measured (M5 asserts it as
  a chunk count) and the piece overhead is not.
- **A pairing where neither direction works becoming common rather than
  exceptional.** Decision 2 assumes the no-path-either-way case is rare enough
  to absorb. If it is not, peers need a relay, which is a different ADR and a
  much larger one.
