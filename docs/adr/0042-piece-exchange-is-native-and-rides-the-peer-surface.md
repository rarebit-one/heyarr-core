# 0042. Piece exchange is Heyarr's own, and rides the peer surface

**Status:** Accepted
**Date:** 2026-08-25

## Context

§25 says the internal storage transport "may eventually use a programmable
BitTorrent implementation when piece-level control is required", and that
"Transmission need not become Heyarr's internal replication engine". M6 is where
that arrives, and nothing had been chosen: `internal/storagefabric/torrent/` has
been a two-line `doc.go` since Milestone 1 and `go.mod` has no torrent
dependency.

Three options were on the table (#262): link an existing Go engine, write a
minimal piece protocol, or drive an external daemon over RPC.

Two facts decided it, and both were already true in the repository.

**The internal transport never speaks to a non-Heyarr peer.** §25 separates
EXTERNAL acquisition — Transmission, talking to the outside world — from
INTERNAL transport, which moves bytes between enrolled peers. §26 then removes
the last reason to be compatible with anyone: peer discovery comes from
authenticated membership, not from trackers or a DHT. So **BitTorrent wire
compatibility buys nothing on this path.** It is a cost with no counterparty.

**The peer surface already carries pieces.** It serves
`GET|HEAD /peer/v1/blobs/{hash}/content` over mTLS with pinned Ed25519 keys
(M4-09, ADR-0012), and §28 makes ranged reads a contract that playback, remote
probing, replication and web-seeding all depend on. ADR-0041 fixed a piece as
fixed-length and aligned from zero. **A piece is a byte range**, and the
authenticated, ranged, redirect-refusing byte channel for it has existed since
Milestone 4.

## Decision

**Piece exchange is Heyarr's own, and it rides the existing peer surface. No
BitTorrent engine is linked in, and no daemon is added.**

Concretely:

- A **piece** is a byte range of the target blob, its geometry derived
  deterministically from the blob so two peers compute the same one without
  agreeing on anything (ADR-0041: a piece table is a transport detail with a
  session's lifetime and is never an identity).
- **Fetching a piece is a ranged GET** against a peer's existing blob content
  route. Nothing new is invented for the byte-carrying half.

  > **Superseded in part, by the work this ADR authorised.** This holds for a
  > blob held WHOLE, and it is what §27's web seed does. It does not work for a
  > blob held in PART, which is the case §23 exists for: the content route
  > promises the blob — a strong `ETag` naming the whole-object digest, a
  > `Content-Length` that is the blob's length, and a `404` meaning *this peer
  > does not have it* — and a node holding a third of the bytes can honour none
  > of the three. Making it try would mean either lying about the `ETag` or
  > inventing partial-content semantics on a route whose contract is
  > deliberately simple (ADR-0013).
  >
  > So there is one new route, `GET /peer/v1/blobs/{hash}/pieces/{index}`, and
  > it is the one way a piece is fetched — whole blob and partial alike. The
  > content route is untouched, still serving the whole-blob pulls replication
  > already does. The rest of this decision stands: the piece routes ride the
  > existing peer surface, on the existing mTLS trust root, with no new
  > credential and no second transport.
- **Two things are new**, and they are what turns a pull into a swarm:
  1. a peer can say **which pieces it holds of a blob it does not hold
     completely**;
  2. a peer can **serve a piece from a partial blob**.
- **Discovery is membership** (§26). The session is handed peers from
  `catalog.BlobSources`, which already returns pinned keys and endpoints.

### What this gives up, stated plainly

**Interoperability with public BitTorrent swarms on the internal path.** A
Heyarr peer will not join a public swarm to replicate between sites, and a
third-party client cannot join Heyarr's. That is not a regression: §26 asks for
exactly this, and external acquisition — where interop genuinely matters — stays
with Transmission and is untouched.

**Everything a mature engine solved that we now do not get**: choking and
rarest-first strategy, NAT traversal, PEX, DHT, encryption negotiation, and
years of hardening against hostile peers. Most of that exists for a *public*
swarm of strangers. This swarm is a handful of machines that have already
authenticated each other with pinned keys, so the threat model that justifies
most of it does not apply — but "we do not need it" is a claim about today's
fabric size, and it is the first thing to revisit if that changes.

### Why not the other two

**An engine linked in** brings a large dependency into a `go.mod` with eighteen
direct entries and none of that size, requires a licence review against
AGPL-3.0-or-later shipping as one static binary (ADR-0016), and then has to be
configured *out* of being a public-swarm client — DHT off, PEX off, LSD off, no
trackers — which is a set of defaults to keep proving off forever. #265 asserts
the absence of public discovery; that assertion is trivially true when there is
no mechanism, and a permanent chore when there is.

**An external daemon** contradicts §25 in as many words and would make a
deployment dependency mandatory on every peer, where ADR-0025 has kept external
services optional. It also puts a second process between Heyarr and its own
bytes on the path where invariant 1 matters most.

## Consequences

- **§27's web seed and §25's transport become the same mechanism.** A web seed
  is a peer serving a range of a blob it holds completely; a swarm member is a
  peer serving a range of one it holds partially. ADR-0013 said blob serving is
  a contract rather than an endpoint, and this is that contract paying off.
- **Serving from a partial blob is the real new capability**, and it is where
  the risk is. M5's staging and verified-prefix machinery (ADR-0035) is what
  makes it possible; a peer must never serve bytes it has not verified, and a
  partial blob has no whole-object digest to verify against — only its pieces.
- **Verification stays two-level and invariant 1 is untouched**: pieces against
  their own hashes during transfer, the whole object against its BLAKE3 digest
  on completion. Nothing about "the engine already checked" enters, because
  there is no engine.
- The chunk manifest keeps its M5 role on either side and takes no part in the
  exchange — reuse planning before, repair after.
- `internal/storagefabric/torrent` is the wrong name for what this is. It should
  be `internal/storagefabric/pieces`, and the placeholder should be renamed
  rather than filled.

## What would make us revisit this

- **A fabric large enough that strategy matters.** Rarest-first and choking earn
  their complexity at swarm sizes this design does not target. A handful of
  peers do not need them; fifty might.
- **A requirement to interoperate on the internal path** — a third-party client
  seeding a Heyarr blob, or Heyarr joining a public swarm to fetch one. §26 says
  no today; if that changes, this decision goes with it.
- **Hostile peers inside membership.** The threat model here is that enrolment is
  the trust boundary (ADR-0012). If a peer can be enrolled and malicious, the
  hardening a mature engine has and this does not becomes worth its cost.

## Status note

**Accepted 2026-08-25**, with one departure recorded rather than left to be
discovered.

This record said a piece would be fetched as a ranged GET on the existing blob
CONTENT route, and that the serving side would need no new surface. That is true
for a blob a peer holds whole and impossible for one it holds partially: the
content route promises a strong `ETag` naming the whole-object digest, a length
that is the blob's length, and a `404` meaning *not here* — and a node holding a
third of the bytes can honour none of the three. **A piece is therefore its own
route**, `GET /peer/v1/blobs/{hash}/pieces/{index}`, alongside an availability
route (#272, #275).

Everything else stands: no engine was linked in, no daemon was added, `go.mod`
gained no torrent dependency, and discovery is membership with nowhere for a
tracker to be configured (#265).
