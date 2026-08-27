# 0044. The client blob route serves partials by blocking on the on-disk bitset

**Status:** Accepted
**Date:** 2026-08-28

## Context

§33 opens *"Clients should continue to consume standard HTTP/HLS"* over content
that has not finished arriving, and §84's Milestone 10 makes progressive playback
a deliverable. M6 built the substrate — `cas.ReadPartialAt` serves bytes out of a
still-assembling staging file, gated by an availability bitset (ADR-0043) — but
wired it to exactly one caller: the peer surface's piece route
(`peersurface.go`). The **client** blob route (`internal/api/blobs`) had no
partial path. `store.Open` returned `cas.ErrNotFound` for a blob held in part,
and the route answered 404. So nothing a *player* talks to could serve what had
not finished arriving, and the milestone's premise — a client speaking ordinary
HTTP over incomplete content — had no mechanism behind it.

Two facts shaped the design, and both were already settled in the repository.

**ADR-0042 established that the peer CONTENT route must not serve partials.** It
promises three things a node holding a third of a blob can honour none of: a
strong `ETag` naming the whole-object digest, a `Content-Length` that is the
blob's length, and a `404` meaning *this peer does not have it*. That is why
piece exchange got its OWN route (`/peer/v1/blobs/{hash}/pieces/{index}`) rather
than overloading the content route. A naive reading says the client content route
is the same route and inherits the same prohibition.

**The piece transfer runs in a different ROLE from the API.** `PullPieces` is
driven by a worker job (`worker/replicateblob.go`); the client blob route is the
controller/API. Invariant 4: *"Roles communicate only through the job table and
HTTP. Never a shared in-process pointer, even inside `heyarr all`."* The live
`pieceSession` holds a condition variable that wakes as pieces land — and it is
in the wrong role for the API route to await directly.

## Decision

**The client blob route serves a blob that is still arriving by presenting
`http.ServeContent` a seekable reader whose `Read` BLOCKS until the requested
bytes have landed, and it learns what has landed by re-reading the on-disk
availability record — never by holding the transfer's session.** The capability
is opt-in per handler and wired only on the client mount.

### Why this is not the prohibition ADR-0042 wrote

ADR-0042's three promises are a statement about a CONSUMER, not about the bytes.
The peer content route's consumer is another peer running replication (ADR-0035),
which reads a chunk boundary it will verify itself and must never be silently
parked waiting for bytes that may never come — for it, a partial genuinely cannot
honour the contract. The client route's consumer is a **player**, which buffers
by design. And for that consumer the three promises are all kept:

- **`Content-Length` is the blob's true length**, taken from the geometry the
  record carries (ADR-0043: a partial's on-disk `Size` is a high-water mark and
  understates; the geometry's size is the real length). Block-then-serve then
  delivers exactly that many bytes.
- **The `ETag` is the whole-object digest**, and it is honest: every byte the
  reader returns is a byte the completed blob will contain, because the pieces
  hash to that digest at `Publish` (invariant 1). A client that caches a
  fully-delivered range from a partial and later revalidates against the whole
  blob gets a match.
- **`404` still means "nothing here"** — the route answers it when there is
  neither a whole blob nor a transfer in flight. "Arriving" is a third state the
  player wants served, not refused.

The peer content route keeps ADR-0042 unchanged: its handler is built with no
partial capability, so it 404s a partial exactly as before. The capability is a
field passed by the client wiring alone, not a type-assertion the peer mount
would also satisfy — the boundary is explicit, not ambient.

### The blob-serving package stays piece-agnostic

§27 and ADR-0013 hold that the blob route serves byte ranges and says nothing
about what they mean; a fixed piece IS a byte range, so serving needs no piece
awareness, and `webseed_test.go`'s guard fails the build if the `internal/api/blobs`
package so much as imports the piece machinery. Progressive serving must not
breach that — the reader genuinely needs to know which bytes have landed, but
"which bytes" is a byte fact, and only the *translation* from a piece
availability record into byte ranges is piece-aware.

So `blobs.PartialSource` is defined in byte terms — `ArrivingSize`, an
`Available(offset) → (until, landed, inflight)`, and `ReadPartialAt` — and the
reader deals only in offsets. The piece decode lives in a controller-side adapter
(`piecePartialSource`), the side of the boundary that already knows what a piece
is: the peer surface's `ReadPiece` does the identical decode a few files over.
The controller wires the adapter into the client handler; the blob-serving
package imports no piece code, and the guard stays green. This is not ceremony —
it is what keeps a future DLNA/HLS adapter, or any other consumer of the byte
contract, from inheriting a piece vocabulary it has no use for.

### Why the signal is the on-disk record, not the session

Invariant 4 forbids the API route from awaiting the worker's condition variable.
The role-legal channel is the one ADR-0043 already made the shared truth: the
`<digest>.pieces` record, which the worker rewrites (atomic rename) as each piece
lands and which lives in the CAS both roles reach. So block-then-serve is a
bounded poll of that record — wait, re-read, re-check — not a subscription. This
is slower than a condition variable by at most the poll interval, on a signal
that changes on network timescales, and it is the only form that survives the
call being a network hop, which Invariant 4 requires of everything.

### The invariant it must hold, stated so it cannot be lost

**A range is served only when the record says its piece has landed.** A hole
reads back as zeroes indistinguishable from data (ADR-0043); serving one ships
bad bytes to a client under the name of the content. So the bitset is consulted
BEFORE every read and the read is bounded to the confirmed piece — the
client-side mirror of the peer surface's `ReadPiece`, resting on the same
ordering rule (a piece is recorded AFTER its bytes are written). This is
sabotage-verified: defeat the `have.Has` gate and the reader serves a hole, and
the test that asserts it never does fails.

## Consequences

- The blob route now has two byte sources behind one contract: the whole blob,
  and — on the client mount only — a partial served progressively. The four
  existing consumers ADR-0013 names are untouched; the peer mount is one of them
  and stays exactly as it was.
- **The transparent transition to a complete replica (§84) falls out of the
  ordering.** The reader checks "is the blob whole now?" before it checks the
  record, so the race between the last piece landing and the transfer publishing
  the whole blob always resolves toward serving the finished replica, and a
  reader that started on a partial finishes from the whole blob once it exists.
- A player asking for `bytes=0-` over a transfer that fetches out of order blocks
  a great deal, because the reader serves sequentially and waits on each gap in
  turn. That is correct but slow, and it is what **time-critical piece priority**
  (the next M10 deliverable) exists to fix: bias the driver toward pieces near
  the playback window so the wait is rare. Progressive serving is correct without
  it and merely fast with it — §84 calls it an optimisation, not a prerequisite.
- Block-then-serve holds a request goroutine open across the wait. That is one
  goroutine per streaming client, which is what net/http already spends on a slow
  download; it is not a new resource class.

## What would make us revisit this

- **A signal cheaper than a poll.** If the worker and the API ever share a
  role-legal event channel — a job-table notification, an event-log tail the API
  already follows — the poll becomes a subscription and the interval goes away.
  Nothing here depends on the interval's value, only on the record being
  re-readable.
- **`Publish` becoming cheaper than a whole read**, which is ADR-0043's own
  trigger: the whole reason a served byte can be trusted is that the pieces are
  re-hashed whole at completion. If that check is ever skipped, this route is
  serving on the strength of a bitset that would then need promoting from hint to
  evidence.
- **A client story that is not plain HTTP** (a DLNA/UPnP or HLS adapter, #202).
  This route is the byte substrate such an adapter sits on; it does not decide
  the protocol the real devices speak, and #202 still owns that.
