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

### Known limitations
- The event stream's `job.succeeded` / `job.failed` events and its live tail
  work across roles, but `/api/v1/system` exposes no head sequence, so a client
  that wants to follow from "now" must replay from zero.
- Linked and vault asset classes exist in the schema and are never written;
  Milestone 1 only ever creates managed assets.
- Milestone 1 ran against exactly one peer, by design. A second Full Peer, real
  transfers between the two, and a proven placement axis arrive in Milestone 4;
  a deployment of one peer remains supported forever, and says so on the wire
  through `placement.unproven`.
