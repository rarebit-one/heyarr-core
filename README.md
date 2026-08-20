# Heyarr

**A self-hosted content lifecycle control plane whose content and encrypted user state
converge across sovereign peers.**

Heyarr is a headless, self-hosted platform for the full content lifecycle —
*intent → discovery → acquisition → ingest → identification → storage → replication → consumption* —
while separately brokering privacy-preserving user state across trusted devices and peers.

It is designed for homelabs, and it should feel less like a collection of media
applications and more like a **distributed content operating system**.

> 🚧 **Status: pre-alpha.** Nothing works yet. Milestone 1 is in progress. The
> [technical specification](docs/spec/) is the authority; the code is catching up.

## Why

The *arr stack plus a media server is several applications, several databases, several
provider integrations, and one implicit rule: the filesystem path is the identity of your
content. Heyarr keeps what that ecosystem got right — monitored state, quality profiles,
deterministic and *explainable* candidate scoring, manual override, external download
clients, hardlink-friendly workflows — and drops what it got wrong:

- a separate application per content type
- one version per title
- filesystem paths as canonical identity
- opaque scoring
- polling as the only integration model

## Architecture

Three planes, deliberately given different consistency models:

| Plane | Owns | Model |
|---|---|---|
| **Control plane** | catalog, desired state, policy, grants, jobs, API | single-writer |
| **Content plane** (Storage Fabric) | BLAKE3-addressed blobs, chunks, replicas | convergent, no primary |
| **Personal state plane** | playlists, ratings, progress, history | encrypted CRDTs, multi-master |

The controller owns decisions, workers own computation, and **peers own and serve bytes**.
A small deployment prefers *full peers* over a primary-server/backup-server model: two
complete sovereign custodians of one logical library, either of which can serve it — and
either of which can rebuild the whole instance if the other site is lost.

Content types are first-class from the start: movies, television, music, audiobooks,
ebooks, PDFs, comics, magazines, papers, documents.

Heyarr deliberately delegates specialised mechanics to established software:
Transmission/qBittorrent for acquisition transport, FFmpeg for audiovisual processing,
Prowlarr for indexer integration.

## Interfaces

Canonical: **JSON/HTTP API**, **MCP**, **event stream (SSE)**, **CLI**.
Compatibility adapters (later): OpenSubsonic, OPDS.

The CLI is a client of the JSON API over the unix socket, and every command has
`--json` alongside its table output. Its reference is generated from the command
tree: [`docs/cli/`](docs/cli/).

Clients consume ordinary HTTP/HLS. BitTorrent, where used, is an *internal transfer
optimisation* — never something a client needs to speak.

## Verifying a build

```bash
make demo
```

`scripts/acceptance.sh` is the executable definition of "this build works". It
runs in well under two minutes on an ordinary laptop, needs no network and no
FFmpeg, and touches nothing outside a temporary directory.

It builds the binary, generates a synthetic library covering every content type,
and then drives the **real** thing end to end: scan, ingest, deduplication,
catalog and replica state over the HTTP API, byte ranges reassembled and checked
against the blob's own BLAKE3 digest, the event log replayed from zero, a
restart, a rescan that must change nothing, a deliberately corrupted blob found
and quarantined by `fsck --deep`, and a garbage collector that must do nothing
at all unless asked.

It runs the whole sequence **twice**: once under `heyarr all`, once with the
controller, worker and peer as three separate processes. Otherwise only one of
the two supported configurations is ever exercised.

If it is green, the build works. If you change something and it goes red, the
failure names what broke and what it expected.

## Roadmap

Milestone 1 is **done** — Heyarr scans a library, brings its bytes under
management, serves them over HTTP with byte ranges, and tells you what it did.
`make demo` is how you check that on your own machine, in about fifteen seconds.

| | Milestone | Delivers | |
|---|---|---|---|
| 1 | Local Heyarr | controller, local full peer, content model, BLAKE3 CAS, scanner, HTTP Range, API, CLI | ✅ |
| 2 | Consumption | playback sessions, direct A/V, publications, ffprobe | |
| 3 | Desired State & Acquisition | DesiredItem, quality profiles, Prowlarr, Transmission, ingest, upgrades | |
| 4 | Second Full Peer | registration, inventory exchange, replication, catalog snapshots, read routing | |
| 5 | Efficient Replication | FastCDC, chunk manifests, resumable transfer, integrity repair | |
| 6 | Cooperative Acquisition | TransferSession, internal BitTorrent transport, web-seeding | |
| 7 | Controller Resilience | continuous backup, restore tooling, cached leases, degraded read mode | |
| 8 | Self-Sovereign Identity | device keys, delegations, grants, pairing, recovery | |
| 9 | Encrypted Personal State | encrypted spaces, wrapped keys, CRDT sync, offline concurrent edits | |
| 10 | Progressive Playback | playback over a partially-available blob | |
| 11 | Compatibility | OpenSubsonic, OPDS, more clients and providers | |

## Licence

[AGPL-3.0-or-later](LICENSE). Contributions under [DCO](CONTRIBUTING.md) sign-off; no CLA.
