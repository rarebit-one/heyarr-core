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
- **Chunk manifests, and the third state `chunked` could not express** — a
  manifest is stored under the blob's whole-object digest, with the chunker
  parameters it was produced under, an ordered chunk sequence and a digest of
  the manifest itself; alongside it a local chunk index answers "where do I
  already hold these bytes" in one query. `blobs.chunked` was a boolean that had
  been `0` on every row in every deployment since Milestone 1, because nothing
  ever wrote it, and §16 needs three answers — a manifest is present, a decision
  was recorded that these bytes never need one, or nobody has decided. The API
  keeps `chunked` (computed honestly, deprecated) and gains `chunk_manifest`,
  which can say all three. Asking a blob's state never generates a manifest, and
  deleting every manifest in the store costs speed and nothing else (ADR-0034).
- **Lazy chunking, by a job** — `chunk_blob` has been in §75's job list since
  Milestone 1 with nothing behind it, and this is the handler. It streams a blob
  once, chunks it, hashes every chunk and hashes the whole object on the same
  pass; if the whole-object digest is not the blob's own name it writes no
  manifest and reports corruption on the existing path, quarantined rather than
  deleted (ADR-0018) — a manifest built from unchecked bytes would be every
  chunk digest correct, describing a file that is not the one it is named after.
  Blobs below 4 MiB are recorded as never needing a manifest rather than left
  undecided: at that size a manifest is a handful of chunks and costs the same
  full read as the transfer it would optimise. Nothing chunks at ingest and
  nothing sweeps the library; the work is enqueued by the convergence cycle that
  has just decided the bytes must cross a network, which is §16's own trigger.
  It is idempotent, keyed on the blob, and emits no event — a manifest existing
  is state, the same argument that keeps `blob.verified` out of the event log.
- **Integrity repair, which never edits a blob in place** — a damaged blob is
  located chunk by chunk against its manifest, a whole replacement is
  reconstructed in the store's private staging area from the intact local
  chunks plus replacements fetched from a peer, the assembled whole-object
  digest is verified, the damaged original is moved to quarantine, and only
  then is the replacement published by the same atomic link every other write
  uses. There is no interval in which a partly assembled file answers to the
  blob's digest: during a repair the addressable blob is the original,
  unchanged, and afterwards it is the repaired one. Quarantine happens *before*
  publication so a crash between them loses the repair rather than the
  evidence. The saving is in the network — the fetch is proportional to the
  damage, the local read and write stay proportional to the blob. A repair that
  cannot complete (no manifest, no reachable peer, a peer whose copy is damaged
  too, an assembly that does not hash to the blob) changes nothing at all and
  says which it was; `heyarr fsck --repair` prints what was repaired, how many
  chunks moved, and why not (ADR-0036, ADR-0018).

### Fixed

- **Binding the local socket no longer poisons the content-addressed store**
  (#151). The unix socket was made owner-only by lowering the process umask to
  `0o177` across the bind. umask is per-process, not per-goroutine, and `heyarr
  all` runs the API and the worker together, so any directory another goroutine
  created inside that window came out `0o600` — `0o750 &^ 0o177` — with no
  search bit, and nothing could ever be written into it again. The store creates
  its shard directories on first use, which is during start-up, which is when
  the socket is bound: one unlucky microsecond left a shard, or the directory
  holding every shard, permanently unwritable, and every later ingest failed
  with `mkdir …: permission denied`. Reported six times over four months as a
  one-in-ten flake and once as twelve ingests out of twelve. The socket is now
  bound inside a private `0o700` directory and renamed onto its published path,
  which closes the window the umask existed for without touching process-global
  state.
- **A store that cannot be written to says so, with evidence** (#151). A
  permission failure anywhere in the store now carries a diagnosis gathered at
  the moment it happened: the parent chain from the store root down, with each
  level's mode and owner, the process umask, and whether an unrelated directory
  can still be created — which is what separates one poisoned shard from a store
  nothing can write to. A store-wide fault is reported once, at `ERROR`, rather
  than once per job that trips over it.

- **Peer liveness reaches a remote peer** (#184). Health is observed rather than
  declared, and in the topology Milestone 4 builds nothing observed it: a remote
  peer holds no bearer token, so it never reached the client API's membership
  guard — the only writer — and the idle probe spoke plain HTTPS, which an mTLS
  listener will not complete a handshake with. A remote peer's stored health
  could not leave `unknown`, which left read routing's health filter and garbage
  collection's durability check running on an input that could not move. The
  peer surface now records liveness on an authenticated inbound request, and the
  probe dials the peer fabric itself, pinned, with this node's certificate.

### Known limitations
- **Everything Milestone 4 proves, it proves on one machine.** The two peers in
  `make demo` are two processes on one host: they share a kernel, a disk, a clock
  and a loopback interface. That establishes the protocol, the pinning, the
  verification, the refusals and the data path. It establishes nothing about a
  real network — partitions, latency, MTU, packet loss, a link that is up but
  lossy — and a green run must not be read as saying the fabric is deployable.
  Peer-to-peer mTLS, pinning, revocation and re-enrolment were exercised between
  two physical machines by hand during the milestone; no automated check does it.
- The garbage-collection refusals that depend on a peer actually ANSWERING —
  Refusal 3, the row that claims `present` against a peer that denies holding
  the bytes — are proven by the Go tests rather than by `make demo`. Peer
  liveness now moves in a peer-surface-only topology (#184), so the demo can
  reach that path; the acceptance section that asserts it is not folded in yet.
- The event stream's `job.succeeded` / `job.failed` events and its live tail
  work across roles, but `/api/v1/system` exposes no head sequence, so a client
  that wants to follow from "now" must replay from zero.
- Linked and vault asset classes exist in the schema and are never written; only
  managed assets are created.
- A deployment of exactly one peer remains supported forever, and says so on the
  wire through `placement.unproven`.
