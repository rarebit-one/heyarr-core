# 0047. A peer says what it speaks, and a web seed is a configuration

**Status:** Accepted
**Date:** 2026-08-25

## Context

§27 asks a transfer to be able to consume pieces from other peers **and** from
"the ordinary Heyarr HTTP Blob endpoint" — a web seed. ADR-0042 settled what
that is: not a foreign HTTP origin, but a member reachable over HTTP that is not
dialling for pieces, served over the peer certificate on the peer surface's
content route.

Milestone 6 built the consuming half. `internal/peer/transfer/webseed.go`
fetches a piece as a ranged GET, handles a `416`, refuses a short read and
refuses an over-long `206`, and `survey` gives a web seed a full claim without
asking it anything. It is tested.

**And nothing constructed one.** `internal/worker/replicateblob.go` built every
candidate as `transfer.Peer(...)`, so `transfer.WebSeed` appeared nowhere
outside a Go test — the mechanism-with-no-caller defect, for the sixth time in
this repository (#266, and see `scripts/claims.list`).

The obvious fix — teach the worker to tell the two apart — ran into a harder
problem. **There was nothing to tell apart.**
`internal/controller/peersurface.go` built `Blobs` and `Pieces` from the same
store in the same call, unconditionally, so every serving peer served both and
no configuration of this tree could produce a node a web-seed candidate was
correct for. A transport choosing between two source kinds was choosing between
one real case and one that could not occur.

That is ADR-0042's own argument — *"it is a cost with no counterparty"* — turned
on §27.

## Decision

**Two parts, and the first is what makes the second worth having.**

### 1. A web seed is a configuration: `peer.serve_pieces: false`

It leaves the content route, the inventory and the manifests untouched, and
refuses the piece routes permanently. A peer fetching a whole blob from such a
node sees no difference at all.

The deployment it exists for is real rather than hypothetical: serving pieces
means an availability question per blob and a request per piece — many small
reads and many small responses where a whole-blob pull is one large sequential
one. On a low-power NAS holding an archive tier that is read rarely and never
acquired to, that is CPU and IOPS spent on a role the node is not there to play.
§19's peer modes already say a fabric is not uniform in what it stores; this
says the same about what it speaks.

The refusal is **501**, not 503 and not 404. A destination must not retry it,
must not conclude the node is unwell, and must not conclude the node lacks the
blob. It is a permanent statement about what this surface does.

### 2. A peer SAYS what it speaks, on the identity route

`GET /peer/v1/identity` gains `speaks`: `blob-content`, `piece-exchange`, or
both. The absence of `piece-exchange` is what makes a member a web seed.

**Why the peer and not the asker.** ADR-0038 makes each peer authoritative for
its own site, so what a peer does is the peer's statement. A field on the asking
node's membership record would be an operator's claim about a machine they are
not, and it would never self-correct when the far node changed.

**Why derived and not declared.** The list is computed from what the server was
BUILT with — it cannot say `piece-exchange` while the piece routes refuse,
because both read the same two fields. That is ADR-0039's "proven by execution"
applied to a peer.

**Why fetched fresh rather than durable and expiring.** ADR-0039 needed
durability and a TTL because a *worker* has no inbound surface to be asked on. A
peer is defined by having one. So there is no table, no expiry to tune, no
migration, and no window in which the record disagrees with reality.

**Why asked rather than inferred from a refusal.** ADR-0042 is right that
probe-and-fallback would make a peer whose piece route is BROKEN
indistinguishable from one that never served pieces — the first is a fault worth
reporting and the second is an ordinary member doing its job. Asking costs
nothing extra: the survey that follows skips the availability question for a web
seed, because a web seed has no availability route and claims every piece by
construction. One round trip per member either way.

**A source that cannot be asked keeps the piece contract.** Downgrading on a
network error is the same mistake as inferring from a refusal, and it would take
the slower contract on the strength of a timeout.

## Consequences

- `transfer.WebSeed` has a caller. §27's endpoint is reachable as a piece source
  from a running binary, and the acceptance demo drives one: its swarm section's
  external source is now configured `serve_pieces: false`, so it serves content
  bytes and **no pieces at all** while two piece peers converge around it. That
  costs the demo nothing — same three nodes, same fixture — and it is a truer
  picture of §23 than a full swarm participant standing in for the outside world.
- **`speaks` is a growing list read across versions.** A node meeting a name it
  does not know must ignore it; one meeting a missing name must not conclude the
  peer is broken. The identity decode is deliberately not strict about unknown
  fields, unlike the availability decode, whose shape is the transport's own and
  ships at both ends together.
- A node that is a web seed still replicates, still reports inventory, still
  serves whole blobs and is still a full member. This is a transport capability,
  not a peer mode (§19) and not a trust boundary (ADR-0012).

## What would make us revisit this

- **A third capability whose absence matters.** Two names is a list; five is a
  negotiation, and a negotiation wants a version rather than a set.
- **A peer whose capabilities change while a session is running.** Fetched fresh
  means fresh at survey time, and a session that outlives a config reload will
  hold a stale kind until it ends. That is currently harmless — the worst case
  is a web seed asked for pieces, which answers 501 and is dropped — and it
  stops being harmless if a capability ever gates something other than a
  transport choice.
