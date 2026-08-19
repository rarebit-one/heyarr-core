# Heyarr — Technical Specification

> Source of truth for the architecture. Extracted from the original PDF
> (2026-08-19) and kept in-repo so it is versioned alongside the code it
> describes. Section numbers (§n) are referenced throughout the codebase,
> the ADRs and the issue tracker.

---

1. Overview
Heyarr is a headless, self-hosted content lifecycle, replication, and consumption platform designed
primarily for homelab deployments.


It manages content across the full lifecycle:


intent → discovery → acquisition → ingest → identification → storage → replication → consumption


while separately brokering privacy-preserving user state across trusted devices and peers.


Heyarr supports:


      • movies
      • television
      • music
      • audiobooks
      • ebooks
      • PDFs
      • comics
      • magazines
      • papers
      • documents
      • extensible future content types

Its canonical interfaces are:


      • JSON/HTTP API
      • MCP
      • event stream
      • CLI

Compatibility adapters may expose:


      • OpenSubsonic
      • OPDS
      • future ecosystem protocols

Heyarr deliberately delegates specialized mechanics to established software where appropriate:

  Transmission / qBittorrent → external acquisition transport
  FFmpeg                              → audiovisual processing
  Prowlarr                            → initial discovery/indexer integration


The system should feel less like a collection of media applications and more like a distributed content
operating system for a homelab.

2. Architectural North Star
Heyarr is:


       A self-hosted content lifecycle control plane whose content and encrypted user state
       converge across sovereign peers.


The architecture has three primary planes:

                                  HEYARR


                       ┌──────────────────┐
                       │   CONTROL PLANE       │
                       │                       │
                       │ catalog               │
                       │ desired state         │
                       │ policy                │
                       │ identity/grants       │
                       │ orchestration         │
                       │ MCP / JSON            │
                       └────────┬─────────┘
                                  │
                           commands/events
                                  │
                   ┌───────────┴───────────┐
                   │                               │
                   ▼                               ▼

             CONTENT PLANE                PERSONAL STATE PLANE

             Content CAS                  Encrypted Spaces
             replicas                     encrypted CRDTs
             acquisition                  device keys

             streaming                      blind sync
             workers


A small Heyarr deployment should prefer full peers rather than a primary-server/backup-server model.

3. Core Architectural Principles
Heyarr follows these rules:


       Semantic objects are not hashes. Bytes are.


       Content addressing identifies immutable data; desired-state policy determines where it
       should exist.


       Full peers converge toward the same logical library rather than treating one site as
       primary and another as backup.


       Encrypted personal state may be fully replicated while remaining unreadable to Heyarr
       infrastructure.


       The controller owns coordinated mutable decisions; content replicas do not require a
       primary node.


       The controller owns decisions, workers own computation, and peers own and serve
       bytes.


       A replica, backup, and cache express different guarantees.


       Desired-state reconciliation drives acquisition, upgrades, replication, and integrity
       repair.


       Normal consumption should remain local to the nearest suitable peer.


       Distribution should improve resilience and locality without forcing distributed
       consensus into ordinary domain operations.

4. Runtime Roles
Heyarr initially ships as one Go executable with explicit roles:

  heyarr controller
  heyarr worker
  heyarr peer


For simple deployments:

  heyarr all


may run all roles.


Roles should be independently runnable as OS processes even when located on the same machine.

5. Full Peer
A Full Peer is the preferred unit of deployment for small multi-site Heyarr installations.


A Full Peer maintains, subject to configured policy:


      • the full canonical content Blob set
      • complete encrypted user-state replicas
      • a local read-oriented catalog snapshot
      • local artwork and metadata required for browsing
      • valid routing/access leases
      • a copy of controller backups
      • direct serving capability
      • replication capability
      • optional worker capability
      • optional external acquisition capability

Example topology:

                           HEYARR LOGICAL INSTANCE

                      Bartley Ridge             Cove
                     ┌──────────────┐       ┌──────────────┐
                     │   Full Peer     │◄─►│    Full Peer       │
                     │                 │    │                   │
                     │ Content CAS     │    │ Content CAS       │
                     │ Private CT      │    │ Private CT        │
                     │ Catalog         │    │ Catalog           │

                    │ HTTP serving │        │ HTTP serving │
                    └──────────────┘        └──────────────┘


There is no primary location for content bytes.

6. Peer Types
Future deployments may support additional peer classes.


Full Peer
Expected to converge toward the complete desired content set.


Partial Peer
Stores only content selected by placement policy.


Cache Peer
Stores disposable local copies for performance.


Archive Peer
Optimized for durable or offline storage rather than interactive reads.


Compute Peer
Provides worker/transcode resources but little or no persistent content storage.


The MVP should optimize for Full Peers first.

7. Controller
The controller owns authoritative mutable coordination state.


Responsibilities:


      • JSON API
      • MCP
      • canonical catalog

      • desired-content state
      • quality and upgrade policy
      • acquisition orchestration
      • peer membership
      • placement policy
      • identity delegations
      • capability grants
      • job scheduling
      • reconciliation
      • device registry
      • active consumption coordination
      • event publication
      • encrypted-state brokerage

The initial implementation is single-writer.


The controller should not routinely move bulk content bytes.

8. Controller Authority vs Peer Authority
Heyarr deliberately separates several authority models.


Controller-authoritative
Examples:

  DesiredItem
  QualityProfile
  Acquisition
  Provider configuration
  Grants
  Peer membership
  Job leases
  Placement policy

Convergent content state
Examples:

  Blob
  Chunk

  Manifest
  Replica


There is no primary content replica.


Convergent encrypted personal state
Examples:

  playlist CRDT changes
  favorites
  annotations
  reading progress
  ratings
  history


These may accept concurrent writes during partitions.

9. Worker
Workers execute leased jobs.


Examples:

  scan
  probe
  hash
  chunk
  identify
  metadata lookup
  search provider
  acquisition supervision
  artifact verification
  ingest
  thumbnail generation
  transcode
  integrity scan
  replication preparation


Workers may run on Full Peers or dedicated compute machines.

10. Technology Stack
Primary implementation:


Go


Suggested components:

  Go
  net/http or chi
  SQLite
  sqlc
  fsnotify
  SSE
  WebSocket where required
  OpenAPI
  MCP SDK
  structured logging


Specialist dependencies:

  FFmpeg / ffprobe
  Transmission initially
  Prowlarr initially

11. Semantic Content Model
Heyarr models content independently of storage representation.

  Work
     │
     ├── Edition
     │     │
     │     └── Asset
     │             │
     │             └── Blob
     │
     └── Edition

Work
The conceptual creative or informational work.


Edition
A specific version, publication, cut, or release.


Asset
A usable local representation.


Blob
A specific immutable byte sequence.

12. Content Types
Content specializations may include:

  Content
  ├── Movie
  ├── Series
  ├── Season
  ├── Episode
  ├── Artist
  ├── Album
  ├── Track
  ├── Audiobook
  ├── Book
  ├── Comic
  ├── Magazine
  ├── Paper
  └── Document


Infrastructure should operate on generic abstractions wherever practical:

  DesiredItem
  ReleaseCandidate
  Policy
  Acquisition

  Artifact
  Asset
  Blob

13. Content-Addressed Blob Identity
Each Blob has a whole-object cryptographic digest.


Initial recommendation:


BLAKE3


Example:

  blake3:87e14f...


The whole-Blob hash is the canonical byte identity.


Benefits:


      • exact deduplication
      • corruption detection
      • replica verification
      • deterministic identity
      • location-independent addressing

14. Blob Immutability
Blobs are immutable.


If a file is retagged or otherwise modified:

  Blob ABC


becomes:

  Blob XYZ

An Asset may continue to represent the same semantic object while referencing a different Blob revision.

15. Content-Defined Chunking
Large Blobs should support Content-Defined Chunking, initially using a FastCDC-style algorithm.


Conceptually:

  Blob
    hash = whole-file BLAKE3
             │
           ▼
       Chunk Manifest
          ├── A
          ├── B
          ├── C
          └── D


Chunking improves:


      • resumable replication
      • partial transfer
      • deduplication across modified files
      • repair of damaged replicas

Chunking does not replace the whole-Blob hash.

16. Lazy Chunking
CDC may be generated lazily.


Initial ingest:

  file
   ↓
  BLAKE3
   ↓
  Blob available


When replication or deduplication requires it:

  Blob
   ↓
  chunking job
   ↓
  CDC manifest


Small Blobs may never require chunk manifests.

17. Storage Fabric
Heyarr's content-addressed storage subsystem is the:


       Heyarr Storage Fabric


Core concepts:

  Blob
  Chunk
  Manifest

  Peer
  Replica
  Backup
  Cache

  PlacementPolicy
  DurabilityPolicy

  ReplicationJob
  IntegrityCheck


It must remain isolated behind stable interfaces.

18. Storage Fabric Boundary
Suggested package boundary:

  internal/storagefabric/
    cas/
    chunking/

     manifests/
     replication/
     placement/
     integrity/
     transport/


The content-management domain must not depend on:


      • physical path layout
      • chunking algorithm
      • local vs remote storage
      • replication transport

The Storage Fabric may eventually become reusable independently of Heyarr, but extraction is not required
initially.

19. Full-Replica Content Policy
A Full Peer defaults to:

  desired_blob_set(peer)
  =
  complete canonical Blob set


Reconciliation compares:

  global desired Blob set
                │
            ▼
  peer inventory
                │
            ▼
  missing Blob/chunks
                │
            ▼
  replication


With Bartley Ridge and Cove configured as Full Peers:

  Blob A → Bartley ✓ Cove ✓
  Blob B → Bartley ✓ Cove ✓
  Blob C → Bartley ✓ Cove ✓


The system converges continuously toward that condition.

20. Content Sync Protocol
Content synchronization should be based on immutable addressable objects.


Conceptually:

  Peer A inventory
           │
         ▼
  compare manifests
           │
           ▼
  missing Blob/chunk IDs
           │
         ▼
  direct transfer
           │
         ▼
  hash verification


Peers should exchange only missing content where possible.

21. Peer-to-Peer Replication
Content transfers should happen directly:

  Bartley ─────────────► Cove


rather than:

  Bartley → Controller → Cove

The controller authorizes and schedules the transfer.


The destination verifies all content hashes independently.

22. BitTorrent Transport
BitTorrent may be used as an optional Storage Fabric transport.


Possible transports:

  DIRECT_HTTP
  BITTORRENT


Future:

  QUIC
  object storage
  other transports


Transport selection should be policy-driven.

23. Cooperative Acquisition
When multiple Full Peers desire the same new Blob, Heyarr may acquire it cooperatively.


Instead of:

  Internet → Bartley
  Bartley → Cove


use:

                          external swarm
                        ↙         ↘
                       Bartley ◄──────► Cove


Both peers exchange completed pieces while also obtaining pieces from external sources.

Once complete, both peers become full local replicas.

24. Acquisition and Replication Convergence
Heyarr should not require acquisition and replication to be strictly sequential.


Traditional model:

  Acquire
     ↓
  Complete
    ↓
  Replicate


Cooperative model:

  Desired Blob
        │
        ├── Bartley acquisition
        └── Cove acquisition
               │
            cooperate
               │
            ▼
  both converge to complete replicas


Internally, both acquisition and replication may create a shared abstraction such as:

  TransferSession


with:

  target Blob
  participants
  available sources
  transport
  priority
  urgency

25. Internal Torrent Transport
External acquisition and internal replication should remain distinct.


External Acquisition
Initially:

  Heyarr
    ↓
  Transmission
    ↓
  configured torrent source

Internal Storage Transport
Potentially:

  Heyarr Peer
     ↓
  programmable torrent engine
     ↕
  other Heyarr peers


The internal transport may eventually use a programmable BitTorrent implementation when piece-level
control is required.


Transmission need not become Heyarr's internal replication engine.

26. Private Swarm Behavior
Internal Heyarr replication torrents should not require public peer discovery.


Peer discovery should come from authenticated Heyarr membership.


Conceptually:

  Controller knows:
  Blob ABC available from:
    Bartley

    Cove
    Archive


The transport layer may bootstrap those peers directly.


Private torrent metadata or equivalent peer authorization may be used.

27. HTTP Web-Seeding
Storage-node HTTP endpoints may also act as web-seed-like sources for BitTorrent replication.


This permits a transfer to consume pieces from:

  other peers
  +
  ordinary Heyarr HTTP Blob endpoint
  +
  external acquisition sources where permitted


The Storage Fabric determines transport strategy.

28. HTTP Range Support
Every peer serving a Blob must support HTTP byte ranges.


Required for:


     • direct playback
     • remote ffprobe
     • random access
     • resumable copying
     • partial verification
     • worker access

Example:

  GET /blobs/:hash
  Range: bytes=0-1048575

29. Remote Probing
Workers should probe media remotely using seekable Range-capable URLs whenever possible.

  worker
     │
     ├── first ranges
     ├── metadata ranges
     └── footer ranges
           │
         ▼
  Heyarr Peer


Whole-Blob materialization should be a fallback.

30. Replica, Backup, and Cache
These remain distinct.


Replica
Expected to be immediately usable and contribute to availability.


Backup
Recovery-oriented copy, possibly offline or versioned.


Cache
Disposable performance copy.


A Full Peer ordinarily maintains full replicas, not merely backups.

31. Read Replicas
Every healthy Full Peer can serve content.


Normal behavior:

  Bartley client → Bartley peer
  Cove client          → Cove peer


Cross-site streaming should be fallback behavior, not the norm.

32. Read Routing
The controller selects a source based on:


       • site locality
       • latency
       • availability
       • health
       • bandwidth
       • transcode capability

A scoped direct URL is returned to the client.


The controller stays out of the content data path.

33. Torrent-Assisted Progressive Playback
Clients should continue to consume standard HTTP/HLS.


They do not need BitTorrent support.


For incomplete locally desired content:

  TV
   │
   │ HTTP/HLS
   ▼
  Bartley Peer
   │
   │ missing byte ranges
   ▼
  internal torrent transport
   │
   ├── Cove
   └── external peers

The local peer may prioritize pieces near the playback window.


This allows progressive playback before full acquisition completes while preserving a conventional client
protocol.

34. Placement Policies
The generic Storage Fabric still supports partial-placement policy.


Example:

  placement:
    replicas: 2
    distinct_failure_domains: true


But for a small multi-site deployment, configuration may simply declare:

  peers:
    bartley:
      mode: full

     cove:
       mode: full


which implies full logical-library convergence.

35. Failure Domains
Peers should advertise physical failure domains.


Example:

  Bartley Ridge → bartley-site
  Cove              → cove-site


Two complete peers at separate sites provide substantially stronger resilience than two copies inside one
machine.

36. Content Durability
Full-peer replication should not be confused with archival backup.


For irreplaceable data, policy may still require:

  Bartley replica
  Cove replica
  +
  offline/versioned backup


For replaceable media:

  Bartley replica
  Cove replica


may itself be sufficient.

37. Encrypted User Artifact Replication
Encrypted user state should default to:

  placement = all trusted Full Peers


because encrypted personal state is typically small and potentially less replaceable than ordinary media.


Examples:


      • playlists
      • ratings
      • annotations
      • reading state
      • listening history
      • watch history
      • favorites
      • recommendation preferences

38. Personal State Authority
Heyarr infrastructure may replicate user state without decrypting it.


The server sees:

  EncryptedSpace ID
  opaque object/change ID
  causal metadata
  ciphertext


It does not see:

  playlist names
  playlist members
  ratings
  annotations
  history contents

39. Encrypted Spaces
Personal data is organized into:

  EncryptedSpace


Examples:

  personal
  family
  shared playlist
  research


Each space has independent encryption keys.

40. Device Identity
Each user device owns its own private key.

A user identity authorizes multiple device keys:

  User Identity
      │
      ├── Phone
      ├── Laptop
      └── Tablet


A device may authenticate against either Full Peer as the same user identity.

41. Wrapped Space Keys
Each authorized device receives a wrapped version of relevant space keys.

                            Space Key
                        /       |
                      ▼          ▼          ▼
                   Phone       Laptop      Tablet


Full Peers store wrapped keys but cannot unwrap them.

42. CRDT-Native Personal State
Private synchronized state should use client-side CRDT semantics.

  Phone encrypted CRDT changes
                   │
                 ▼
            Bartley Peer
                 │
            peer replication
                  ▼
               Cove Peer
                   │
               ▼
  Laptop downloads encrypted changes
                   │

               ▼
  local CRDT merge


The infrastructure transports opaque changes.


Clients perform semantic merges.

43. Personal-State Multi-Master Operation
Unlike control-plane state, encrypted personal state may remain writable on multiple peers during a site
partition.


Example:

  Bartley partition:
    Phone writes encrypted change A

  Cove partition:
    Tablet writes encrypted change B


When connectivity returns:

  A ↔ B replication
     ↓
  authorized clients merge
     ↓
  converged CRDT state


This is an intentional multi-master model.

44. Personal-State Sync Protocol
State synchronization is distinct from CAS synchronization.


CAS Sync
Optimized for:

  large immutable blobs
  hashes
  chunks
  manifests

State Sync
Optimized for:

  small encrypted CRDT changes
  causal heads
  snapshots
  compaction


They should share transport/authentication infrastructure where useful but remain separate protocols.

45. Personal-State Replication
Every trusted Full Peer should maintain:


     • all encrypted space metadata
     • wrapped keys
     • encrypted CRDT changes
     • encrypted snapshots
     • compaction metadata

This permits user devices to sync against whichever site is reachable.

46. Local-First User Experience
A user device should not need to know which Heyarr site is authoritative for personal state.


Conceptually:

  Phone
    ↓
  discover reachable Heyarr peer
    ↓
  authenticate cryptographically

    ↓
  sync encrypted spaces


Either Bartley Ridge or Cove can provide the user's ciphertext.

47. Shared Encrypted Spaces
Shared playlists or other group artifacts use their own keys.

  Family Playlist Space
           │
           ├── wrapped for User A
           └── wrapped for User B


The infrastructure can replicate the state fully without plaintext access.

48. Control Plane State
Control-plane state is different from both content and user CRDT state.


Examples:

  desired content
  acquisition jobs
  policy
  leases
  grants
  peer membership
  provider config


This state remains single-writer initially.


Do not attempt active-active replication of a live SQLite database.

49. Controller Database
Default:

SQLite


Requirements:


     • WAL mode
     • migrations
     • foreign keys
     • durable jobs
     • continuous backup
     • safe restore
     • periodic checkpointing

50. Controller Backups on Full Peers
Each trusted Full Peer should maintain an up-to-date controller backup or recovery stream.


Conceptually:

                        Controller SQLite
                             │
                       backup stream
                       /
                     ▼                  ▼
                Bartley Peer         Cove Peer


This backup is different from the peer's local read-oriented catalog snapshot.

51. Disaster Recovery
A surviving Full Peer should contain enough information to bootstrap recovery.


Target command:

  heyarr recover --from-peer cove


Recovery inputs may include:


     • latest controller backup
     • complete Content CAS
     • encrypted personal-state history

     • peer membership metadata
     • catalog snapshot

The recovery process reconstructs a new authoritative controller.

52. Catalog Snapshot
Full Peers should keep a local materialized read snapshot containing enough information for degraded
operation.


Potential contents:

  Works
  Editions
  Assets
  Blob mappings
  library organization
  basic metadata
  artwork references
  known device access leases


The snapshot should not be treated as independently writable control state.

53. Degraded Operation
If the controller disappears, a Full Peer should continue useful read-oriented behavior.


Target behavior:


                          Operation                        During controller outage

                          Browse known content             Yes

                          Search local catalog             Yes

                          Stream local content             Yes

                          Read books/PDFs                  Yes

                          Fetch encrypted user state       Yes

                          Accept personal CRDT sync        Preferably yes

                          New acquisition                  No

                          Operation                        During controller outage

                          Change quality policy            No

                          Change grants                    No

                          Delete replicas                  No

The system should become conservative rather than unavailable.

54. Read Authorization During Controller Outage
Peers may cache signed access/routing leases.


A lease may bind:

  Principal
  Resource
  Capabilities
  Expiry


Long-lived device identity plus short/medium-lived cached grants can permit safe degraded read access.

55. Desired Content
A DesiredItem expresses:


       This content should exist under these conditions.


Example:

  {
      "content_id": "episode_456",
      "quality_profile": "living-room",
      "monitor": true
  }

56. Desired Full-Peer Convergence
For content satisfying a DesiredItem:

  DesiredItem
     ↓
  Asset
     ↓
  Blob
     ↓
  Full Peer target set
      │
      ├── Bartley
      └── Cove


Satisfaction can be evaluated separately at two levels:

  content satisfied
  +
  placement satisfied


Example:

  Content exists                ✓
  Bartley replica               ✓
  Cove replica                  ✗

  Overall state:
  available but placement incomplete

57. Reconciliation Domains
Heyarr uses reconciliation consistently.


Content reconciliation

  DesiredItems vs Assets

Quality reconciliation

  QualityProfile vs available Assets

Peer convergence

  Desired Blob set vs peer inventory

Integrity reconciliation

  Expected hashes vs verified bytes

User-state synchronization

  Known CRDT heads vs missing encrypted changes

58. Acquisition
Heyarr decides what should be acquired.


External clients perform protocol-specific transfer mechanics.


Initial:

  Transmission


Future:

  qBittorrent
  SABnzbd
  NZBGet
  HTTP

59. Provider Registry
Provider configuration is centralized.

  ProviderRegistry
  ├── credentials
  ├── capabilities
  ├── routing
  ├── health
  └── configuration


Content types do not maintain separate provider registries.

60. Lessons Retained from *arr
Heyarr preserves:


     • monitored/wanted state
     • quality profiles
     • upgrades
     • deterministic candidate scoring
     • explainable rejection reasons
     • manual search
     • manual override
     • external download clients
     • completed-download handling
     • hardlink/reflink-friendly workflows
     • centralized provider management
     • operational visibility

61. *arr Constraints Avoided
Heyarr avoids:


     • separate applications per content type
     • duplicated integrations
     • one version per title
     • filesystem paths as canonical identity
     • tags as hidden control policy
     • opaque scoring
     • content-specific job systems

      • polling as the only integration model

62. Quality Profiles
Profiles define:

  acceptable
  preferred
  fully satisfied


Example:

  {
      "accept": {
         "minimum_resolution": 1080
      },
      "prefer": {
         "hevc": 20,
         "hdr": 10
      },
      "terminal": {
         "resolution": 2160,
         "source": "remux"
      }
  }

63. Release Candidate Evaluation
ReleaseCandidate evaluation is deterministic and inspectable.


Example:

  {
      "accepted": true,
      "score": 85,
      "reasons": [
        {
          "rule": "minimum_resolution",
          "result": "pass"

          },
          {
               "rule": "prefer_hevc",
               "result": "bonus",
               "score": 20
          }
      ]
  }

64. Acquisition State Machine

  MISSING
     ↓
  SEARCHING
     ↓
  CANDIDATES_FOUND
     ↓
  SELECTED
      ↓
  QUEUED
     ↓
  DOWNLOADING
     ↓
  VERIFYING
     ↓
  INGESTING
     ↓
  AVAILABLE
     ↓
  CONTENT_SATISFIED
     ↓
  PLACEMENT_CONVERGING
      ↓
  FULLY_SATISFIED


This explicitly distinguishes obtaining usable content from successfully replicating it to all required Full
Peers.

65. Ingest
Ingest means:

      These bytes now exist; bring them under Heyarr management.


Sources:


     • completed acquisition
     • local filesystem
     • upload
     • scanner
     • watched folder
     • Calibre
     • Syncthing
     • rclone
     • another Heyarr peer
     • physical-media rip

66. Ingest Pipeline

  Artifact
     ↓
  detect
     ↓
  probe
     ↓
  identify
     ↓
  verify
     ↓
  Work / Edition resolution
     ↓
  Asset
     ↓
  Blob hash
     ↓
  CAS
     ↓
  optional CDC
     ↓
  Full-Peer convergence

67. Consumption Model
Consumption includes:


      • watching
      • listening
      • reading
      • continuing
      • queueing

General abstraction:

  ConsumptionSession

68. Playback
The playback planner considers:

  Asset
  +
  Device capabilities
  +
  local/remote replica availability


and chooses:

  DIRECT
  REMUX
  TRANSCODE


Local replica use is strongly preferred.

69. Publications
Heyarr stores and serves but does not render:

  EPUB
  PDF

  CBZ
  CBR


Clients remain responsible for rendering.


Heyarr manages:


     • metadata
     • storage
     • replication
     • access
     • reading-state integration

70. Compatibility APIs
Potential adapters:

  OpenSubsonic → music
  OPDS                → publications


Neither protocol defines Heyarr's canonical data model.

71. MCP
MCP exposes semantic actions.


Examples:

  search_content
  want_content
  monitor_content

  search_releases
  explain_release
  acquire_release

  get_peer_status
  get_replica_status
  sync_peer
  verify_blob

  play_content
  transfer_playback

  get_missing_content
  get_upgrade_candidates

72. MCP and Private State
Controller-side MCP cannot decrypt user artifacts.


Agents connected only to Heyarr may operate on:


     • library content
     • acquisition
     • peer status
     • playback
     • explicitly server-readable state

Private playlists and similar artifacts remain inaccessible unless explicitly surfaced by an authorized user
device.

73. Personal MCP
Future first-party clients may expose:

  Personal MCP


to authorized agents.

                              Agent
                          /
                       ▼             ▼
                 Heyarr MCP       Personal MCP

               library            playlists
               playback           history
               acquisition        ratings
               peers              annotations

74. OS Security
Use OS-level controls for containment:


      • Unix service accounts
      • groups
      • POSIX ACLs
      • restricted mounts
      • cgroups/container limits

Application-level capabilities remain necessary for caller authorization.

75. Job Model
Jobs include:

  reconcile_content
  reconcile_quality
  reconcile_peer

  search_release
  poll_download

  probe
  ingest
  hash_blob
  chunk_blob

  replicate_blob
  verify_replica
  torrent_transfer

  transcode
  metadata


Jobs are:


      • durable
      • retryable
      • lease-based
      • capability-routed
      • idempotent where practical

76. Events
External event transport initially uses SSE.


Categories include:

  content.*
  desired.*

  acquisition.*
  ingest.*

  blob.*
  peer.*
  replica.*
  sync.*

  playback.*

  private_state.*


  job.*
  system.*

77. JSON API
Suggested hierarchy:

  /api/v1

  /content
  /works
  /editions
  /assets
  /libraries

  /wanted
  /reconciliation

  /releases
  /acquisitions
  /ingest

  /blobs
  /manifests
  /chunks


  /peers
  /replicas
  /backups
  /caches
  /placement-policies
  /transfers

  /devices
  /playback
  /consumption

  /principals
  /grants

  /encrypted-spaces
  /private-state

  /jobs
  /events
  /system

78. Package Architecture
Suggested repository:

  cmd/
    heyarr/

  internal/

    domain/
      content/
      desired/
      policy/
      acquisition/
      ingest/
      playback/
      identity/

controller/

worker/

peer/
  catalog/
  degraded/

storagefabric/
  cas/
  chunking/
  manifests/
  replication/
  torrent/
  placement/
  integrity/
  transport/

personalstate/
  protocol/
  crdt/
  encryption/
  spaces/
  replication/

api/
  http/
  mcp/
  opds/
  subsonic/

providers/
downloads/

media/
  ffmpeg/
  probe/


jobs/
events/

persistence/
  sqlite/

config/

79. Initial Controller Data Model
Likely entities:

  works
  editions
  external_ids

  libraries

  desired_items
  quality_profiles
  policy_rules

  providers

  releases
  release_evaluations

  acquisitions
  download_jobs
  artifacts

  assets

  blobs
  blob_manifests
  chunks

  peers
  peer_capabilities
  peer_snapshots

  replicas
  backups
  caches

  placement_policies
  durability_policies

  devices
  playback_sessions
  queues

  principals
  credentials

  delegations
  grants

  encrypted_spaces
  wrapped_keys
  private_state_heads

  jobs
  events


Encrypted CRDT content itself may live in the peer state store rather than relational application tables.

80. Initial Two-Site Topology
For Bartley Ridge and Cove:

                                    CONTROLLER
                                     │
                        policy / state / orchestration
                                            │
                    ┌───────────────┴───────────────┐
                    │                                           │
                    ▼                                           ▼

             BARTLEY RIDGE                                   COVE

                Full Peer                                Full Peer
                ─────────                                ─────────
                Content CAS             ◄══════►         Content CAS
                CDC chunks              ◄══════►         CDC chunks
                Encrypted state         ◄══════►         Encrypted state
                Catalog snapshot        ◄──────►         Catalog snapshot
                Controller backup ◄──────►               Controller backup


                HTTP serving                                 HTTP serving
                Torrent transport                            Torrent transport
                optional worker                              optional worker


Normal client traffic remains site-local.


Inter-site traffic primarily consists of convergence.

81. Two-Site Failure Behavior
Inter-site link failure
Both sites continue:

  browse
  stream
  read
  serve encrypted user state
  accept local CRDT updates


Content acquisition and policy operations depend on controller reachability.


Encrypted personal state may continue independently and merge later.


Bartley site failure
Cove retains:


     • full content library
     • full encrypted user-state replica
     • controller backup
     • read catalog snapshot


Cove site failure
Bartley provides the same.

82. Recovery Target
A surviving Full Peer should be sufficient to rebuild a viable Heyarr deployment.


Conceptually:

  heyarr recover --from-peer <peer>


The system should recover:

  control-plane backup
  content CAS
  encrypted personal state
  catalog snapshot
  peer identity/configuration


External provider credentials may require protected backup depending on deployment policy.

83. Scope Discipline
Every feature must belong clearly to one of:

  CONTROL PLANE
  CONTENT STORAGE FABRIC
  PERSONAL STATE PLANE
  EXTERNAL SPECIALIST


Examples:

  Torrent protocol mechanics
  → external acquisition engine / Storage Fabric transport

  Video encoding
  → FFmpeg

  Desired content policy
  → control plane

  CAS replication
  → Storage Fabric

  Playlist merge
  → personal-state client

  Encrypted playlist transport
  → personal-state replication

84. Revised Delivery Sequence
Milestone 1 — Local Heyarr
Deliver:


      • controller
      • local Full Peer
      • Work / Edition / Asset / Blob
      • BLAKE3
      • local CAS
      • scanner
      • HTTP Range
      • JSON API
      • CLI

Milestone 2 — Consumption
Deliver:


      • playback sessions
      • direct audio/video playback
      • publication access
      • ffprobe
      • basic FFmpeg integration
      • device capabilities

Milestone 3 — Desired State and Acquisition
Deliver:


      • DesiredItem
      • quality profiles
      • Prowlarr
      • Transmission
      • candidate evaluation
      • ingest
      • upgrade workflow

Milestone 4 — Second Full Peer
Deliver:


      • peer registration
      • inventory exchange
      • full-library replication
      • direct peer transfer
      • catalog snapshots
      • local read routing

This is the first true two-site Heyarr deployment.

Milestone 5 — Efficient Replication
Deliver:


      • FastCDC
      • chunk manifests
      • resumable replication
      • chunk reuse
      • integrity repair

Milestone 6 — Cooperative Acquisition
Deliver:


      • TransferSession
      • internal BitTorrent transport
      • peer-to-peer piece exchange
      • cooperative Full-Peer acquisition
      • HTTP web-seed integration

Milestone 7 — Controller Resilience
Deliver:


      • continuous SQLite backup
      • backups replicated to Full Peers
      • restore tooling
      • cached routing/access leases
      • degraded local-read mode

Milestone 8 — Self-Sovereign Identity
Deliver:


      • device keys
      • delegations
      • grants
      • pairing
      • recovery

Milestone 9 — Encrypted Personal State
Deliver:


      • EncryptedSpace
      • wrapped keys
      • CRDT synchronization
      • multi-peer encrypted-state replication
      • offline concurrent edits
      • snapshots/compaction

Milestone 10 — Progressive Torrent-Assisted Playback
Deliver:


      • local HTTP playback over partially available Blob
      • time-critical piece priority
      • peer-assisted missing-range retrieval
      • transparent transition to complete local replica

This remains an optimization, not a prerequisite for normal playback.

Milestone 11 — Compatibility
Deliver:


      • OpenSubsonic
      • OPDS
      • additional acquisition clients
      • additional provider adapters

85. Final Architectural Model

                                       HEYARR

                             SINGLE-WRITER CONTROL
                                      │
                             desired state / MCP
                                policy / jobs
                                        │
                ┌──────────────────┴──────────────────┐
                │                                                │

         BARTLEY FULL PEER                                 COVE FULL PEER
                │                                                │
                │                                                │
         Content CAS       ◄══════════════════════► Content CAS
                │                CAS sync                        │
                │                                                │
         Encrypted         ◄══════════════════════► Encrypted
         user state              CRDT sync           user state
                │                                                │
              │                                                 │
         local clients                                     local clients


Heyarr's preferred two-site model is therefore:


       one logical library, multiple complete sovereign peers.


The content plane converges through immutable content addressing.


The personal-state plane converges through encrypted CRDT synchronization.


Only the coordinated mutable control plane initially requires a leader.


The key final rules are:


       A Full Peer is not a backup of another peer; both are equal custodians of the logical
       library.


       Every Full Peer should be able to serve the library locally without depending on another
       site.


       Encrypted user artifacts should normally replicate to every trusted Full Peer.

Content acquisition can itself be cooperative, allowing peers to become replicas while
the content is still arriving.


BitTorrent is an internal transfer optimization; clients continue to consume ordinary
HTTP/HLS/OPDS/OpenSubsonic interfaces.


A surviving Full Peer should contain enough data to reconstruct the Heyarr instance
after loss of the other site and controller.


The control plane coordinates convergence; it does not need to own the bytes or
understand private user data.


