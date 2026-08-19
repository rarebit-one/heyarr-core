# Architecture

The authority is [the technical specification](spec/heyarr-spec.md). This page is
the orientation you want before reading code, and the map from spec sections to
packages.

## Three planes, three consistency models

Heyarr's central design choice is that it does *not* have one consistency model.

| Plane | Owns | Model | Why |
|---|---|---|---|
| **Control** | catalog, desired state, policy, grants, jobs, peer membership | single-writer SQLite | coordinated mutable decisions need one writer (§48) |
| **Content** (Storage Fabric) | blobs, chunks, manifests, replicas | convergent, no primary | content addressing makes "which copy is authoritative" a non-question (§8) |
| **Personal state** | playlists, ratings, progress, history, annotations | encrypted CRDTs, multi-master | must stay writable during a partition, and unreadable to the server (§38, §43) |

Only the control plane needs a leader. That is what makes degraded operation
possible: if the controller vanishes, a Full Peer keeps browsing, streaming,
reading and serving encrypted user state — it just stops accepting new
acquisitions and policy changes (§53). The system becomes conservative rather
than unavailable.

## Roles

`controller` decides, `worker` computes, `peer` serves bytes. One binary, wired
once (ADR-0002). They talk through the job table and HTTP, never through shared
memory — even in `heyarr all`.

## The content model

```
Work ── Edition ── Asset ── Blob
```

A **Work** is the conceptual thing. An **Edition** is a specific version, cut or
release. An **Asset** is a usable local representation. A **Blob** is an
immutable byte sequence identified by its BLAKE3 digest.

Assets carry a **source class** that says what Heyarr promises about them
(ADR-0020): `managed` bytes live in the CAS with every guarantee; `linked` bytes
stay where the user keeps them and have **no Blob at all**, so they are
catalogued and playable but never replicated, verified or garbage-collected;
`vault` bytes are in the CAS as ciphertext the infrastructure cannot read
(ADR-0021).

The load-bearing sentence from §3: *semantic objects are not hashes; bytes are.*
Content addressing identifies immutable data; desired-state policy determines
where it should exist. Those are separate questions, and most of the design
follows from keeping them separate.

## Package map

| Spec | Package |
|---|---|
| §7 controller | `internal/controller` |
| §9 worker | `internal/worker` |
| §11–12 content model | `internal/domain/content` |
| §13–16 hashing, chunking | `internal/hashing`, `internal/storagefabric/chunking` |
| §17–22 storage fabric | `internal/storagefabric/**` |
| §28–29 range serving, remote probe | `internal/api/http`, `internal/media/probe` |
| §37–47 personal state | `internal/personalstate/**` |
| §49–54 controller DB, degraded mode | `internal/persistence/sqlite`, `internal/peer/degraded` |
| §55–64 desired state, acquisition | `internal/domain/{desired,policy,acquisition}` |
| §65–66 ingest | `internal/domain/ingest`, `internal/scanner` |
| §67–69 consumption | `internal/domain/playback` |
| §70–73 APIs | `internal/api/{http,mcp,opds,subsonic}` |
| §75–76 jobs, events | `internal/jobs`, `internal/events` |

## Two boundaries enforced by CI

Spec §18 and §83 are not review conventions here; they are `depguard` rules in
`.golangci.yml`:

- The content domain cannot import `os`, `path/filepath`, `database/sql`, the
  persistence layer, or the CAS. It does not know where or how bytes are stored.
- The Storage Fabric cannot import the domain or the API. It stays extractable.

Crossing either line is a signal that an interface is missing, not that an
exclusion is needed.
