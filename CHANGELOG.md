# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Heyarr is pre-1.0: anything may change, and the on-disk formats are not yet
stable.

## [Unreleased]

### Added

**Milestone 1 — Local Heyarr.** One machine, one peer, real bytes. Heyarr scans
a library, brings its files under management, serves them over HTTP with byte
ranges, and reports what it did. `make demo` verifies a build end to end in
about fifteen seconds.

- **Configuration, roles and lifecycle** — layered config (file, `HEYARR_`
  environment, flags), `controller`/`worker`/`peer` as independently runnable
  processes and as `heyarr all`, graceful shutdown that drains in-flight work.
- **Persistence** — SQLite with a single-writer pool, WAL, verified pragmas, and
  refusal to start against a schema newer than the binary.
- **Content model** — works, editions, assets, blobs, replicas and peers, with
  the multi-peer and managed/linked/vault shapes present from the start so later
  milestones are additions rather than rewrites.
- **BLAKE3 content addressing** — a content-addressed store with crash-safe
  writes, quarantine, and materialisation that tries reflink, then hardlink,
  then a copy.
- **Durable job queue** — leased, retried with jittered backoff, capability-
  routed, idempotent, with a reaper for leases whose worker died.
- **Append-only event log** — every state transition recorded, with gapless
  reconnection from any sequence number.
- **Ingest** — the single path bytes enter Heyarr. Idempotent: the same file
  twice is one blob, one asset, one replica; two paths with identical bytes are
  one blob and two assets.
- **Naive identifier** — path and filename to Work and Edition for films,
  series, music and books, recording which rule decided, so Milestone 3 can
  re-resolve deliberately rather than guess.
- **Scanner** — a fingerprint cache that makes a rescan of an unchanged library
  read nothing at all, and enqueues ingest only for what is new or changed.
- **HTTP API** — chi, RFC 9457 problem documents, argon2id bearer tokens with
  scopes, keyset pagination, an SSE event stream, and a hand-written OpenAPI
  document with a route↔spec parity test in both directions.
- **Blob byte serving** — `Range`, multi-range, `If-Range`, 206 and 416, with
  memory flat in blob size: 20 GiB costs the same as 1 MiB.
- **Integrity** — `verify_blob`, `heyarr fsck [--deep]`, and refcounted garbage
  collection that is dry-run by default and quarantines corrupt bytes rather
  than deleting them.
- **CLI** — libraries, scans, works, assets, blobs, jobs, peers and events, all
  with `--json`, over a unix socket by default. `scan --wait` exits non-zero
  when the work failed.
- **Packaging** — goreleaser binaries with checksums, SBOMs and provenance, a
  distroless container image, and a hardened systemd unit scoring 1.3 on
  `systemd-analyze security`.
- Repository scaffold: the package tree from spec §78, build and lint tooling,
  CI, and the initial architecture decision records.

**Milestone 4 — Second Full Peer.** A blob that existed on one peer exists,
verified, on two — and the system refuses to delete the last copy when it cannot
prove otherwise. `make demo` stands two peer processes up and drives the whole
arc twice: once with each node as `heyarr all`, once with each node as three
separate role processes (ADR-0002).

- **Peer identity and enrolment** — an Ed25519 identity per node, and mutually
  authenticated TLS between peers with no CA and no PKI anywhere in the path.
  Both ends pin: a peer is refused unless the certificate it presents carries the
  key the local membership record holds. Enrolment is operator-mediated and
  explicit — no discovery, no join token, no trust on first use — and revocation
  is deletion of the membership row, which closes the whole API on the connection
  already open rather than one endpoint somebody remembered to guard.
- **The peer surface** — a second listener with its own trust root, carrying
  identity, controller attachment, inventory reports, catalog snapshots and blob
  content. The client API's bearer token is not accepted there and a peer is
  never issued one.
- **Inventory exchange** — what a peer's disk holds, reported through the same
  door every other peer uses and recorded against the peer its certificate
  proved. `replicas` stays what the controller BELIEVES; an inventory is what was
  observed; the time a claim was last confirmed is stored beside it, because the
  difference between a fact and a fact about the past is what later decisions
  turn on.
- **Replication** — the destination pulls, verifies the bytes against their
  digest itself, and only then claims a replica. The controller is not on the
  byte path and there is no route that would put it there.
- **The peer surface records what it serves** — a node now logs which peer read
  which blob off it, and by which method. A Full Peer had no record of this at
  all: everything else in the fabric is written from the reader's side, so a
  source being drained by a peer was visible nowhere on the machine sending the
  bytes. It is also the only thing that tells the two surfaces apart from
  outside — the client API's blob route and the peer fabric's share a handler by
  design and its metrics label them identically, which made "the controller
  carried no bytes" unmeasurable rather than merely unmeasured. GET is logged at
  info and HEAD at debug, so the volume tracks the bytes.
- **Placement** — `PLACEMENT_CONVERGING` is reachable by a running system for the
  first time. Satisfaction names which peers are missing a blob, and distinguishes
  a gap replication is closing from bytes that are on nobody at all.
  `placement.unproven` is computed rather than assumed, and answers `false` the
  moment a second peer is required.
- **Peer health** — reachability observed from interactions rather than declared,
  edge-triggered, with `unknown` deliberately not a synonym for reachable.
- **Read routing** — a client's own site is preferred, and the reasons the other
  candidates lost are reported rather than left to be inferred.
- **Garbage collection confirms placement before unlinking** — ADR-0018's
  deferred precondition. A blob's last local copy is not removed unless another
  peer can be shown, affirmatively and recently, to hold it. An unreachable peer,
  a stale claim, a `replicas` row the peer contradicts (corrected to `missing` on
  the way past), a collector with no way to ask, and a controller that cannot be
  reached (§53) each spare the blob and say so by name, in `gc --json` and in the
  plain output. A catalog that does not describe the store in front of it refuses
  the untracked sweep outright — an empty database against a populated store used
  to unlink the library. A single-peer deployment still collects, recording
  `sole_peer` as the basis: refusing there would mean no single-node Heyarr could
  ever reclaim a byte.
- **Acceptance** — the milestone arc asserted step by step against two running
  nodes under both process models, including the controller serving no blob bytes
  during a transfer (against an instrument first proven to fire) and garbage
  collection sparing the last copy with the remote peer stopped.

**Milestone 5 — Efficient Replication.** In progress.

- **Content-defined chunking** — FastCDC in `internal/storagefabric/chunking`,
  streaming and pure: no filesystem, no database, no CAS, no domain, and a
  memory footprint of one buffer whatever the input size. Chunk boundaries come
  from a rolling hash of the content, so inserting a byte at the front of a file
  moves the boundaries around the insertion and leaves the rest of the stream
  cutting where it did — measured at 99.9% of chunk digests surviving a one-byte
  shift, against 0% for a fixed-size chunker over the same bytes. Boundaries are
  pinned by golden fixtures asserted on Linux, macOS and Windows, because two
  peers that chunk one blob differently deduplicate nothing and nothing goes red
  to say so. Chunks are not identity; a blob is still its whole-object BLAKE3
  digest (ADR-0005).

### Known limitations
- **Everything Milestone 4 proves, it proves on one machine.** The two peers in
  `make demo` are two processes on one host: they share a kernel, a disk, a clock
  and a loopback interface. That establishes the protocol, the pinning, the
  verification, the refusals and the data path. It establishes nothing about a
  real network — partitions, latency, MTU, packet loss, a link that is up but
  lossy — and a green run must not be read as saying the fabric is deployable.
  Peer-to-peer mTLS, pinning, revocation and re-enrolment were exercised between
  two physical machines by hand during the milestone; no automated check does it.
- Peer liveness is recorded on the client API and not on the peer surface, so
  where peers only ever meet over the peer surface a remote peer's stored health
  stays `unknown` until the health beat probes it — and the beat's prober speaks
  plain HTTPS, which an mTLS listener will not complete. Garbage collection is
  correct but conservative under that condition: it spares rather than unlinks,
  which is the safe direction, but the refusals that depend on a peer actually
  answering are proven by the Go tests rather than by `make demo`.
- The event stream's `job.succeeded` / `job.failed` events and its live tail
  work across roles, but `/api/v1/system` exposes no head sequence, so a client
  that wants to follow from "now" must replay from zero.
- Linked and vault asset classes exist in the schema and are never written; only
  managed assets are created.
- A deployment of exactly one peer remains supported forever, and says so on the
  wire through `placement.unproven`.
