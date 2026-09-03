# Architecture decision records

Short records of decisions that were expensive to make and would be expensive to
reverse. Each states a decision, why, and — where it matters — what would make us
revisit it.

An ADR that merely describes the code is not worth having.

| # | Decision | Status |
|---|---|---|
| [0001](0001-module-path-and-repository-identity.md) | Module path and repository identity | Accepted |
| [0002](0002-one-binary-roles-as-subcommands.md) | One binary, roles as subcommands | Accepted |
| [0003](0003-the-control-plane-is-single-writer-sqlite.md) | The control plane is single-writer SQLite | Accepted |
| [0004](0004-pure-go-sqlite-driver.md) | Pure-Go SQLite driver | Accepted |
| [0005](0005-blake3-whole-object-digest-is-the-canonical-byte-identity.md) | BLAKE3 whole-object digest is the canonical byte identity | Accepted |
| [0006](0006-the-cas-owns-bytes-paths-are-not-identity.md) | The CAS owns bytes; paths are not identity | Accepted |
| [0007](0007-storage-fabric-package-boundary.md) | Storage Fabric package boundary | Accepted |
| [0008](0008-durable-leased-capability-routed-jobs.md) | Durable, leased, capability-routed jobs | Accepted |
| [0009](0009-events-are-first-class-from-milestone-1.md) | Events are first-class from Milestone 1 | Accepted |
| [0010](0010-the-peer-model-exists-from-milestone-1-with-exactly-one-peer.md) | The peer model exists from Milestone 1, with exactly one peer | Accepted |
| [0011](0011-milestone-1-authentication-scoped-bearer-tokens-loopback-by-default.md) | Milestone 1 authentication: scoped bearer tokens, loopback by default | Accepted |
| [0012](0012-peer-to-peer-authentication-mtls-over-ed25519-peer-identity.md) | Peer-to-peer authentication: mTLS over Ed25519 peer identity | Accepted |
| [0013](0013-blob-serving-is-a-contract-not-an-endpoint.md) | Blob serving is a contract, not an endpoint | Accepted |
| [0014](0014-ingest-materialisation-reflink-then-hardlink-then-copy.md) | Ingest materialisation: reflink, then hardlink, then copy | Accepted |
| [0015](0015-openapi-is-hand-written-and-contract-tested.md) | OpenAPI is hand-written and contract-tested | Accepted |
| [0016](0016-licensing-agpl-30-or-later-dco-no-cla.md) | Licensing: AGPL-3.0-or-later, DCO, no CLA | Accepted |
| [0017](0017-time-identifiers-and-determinism.md) | Time, identifiers and determinism | Accepted |
| [0018](0018-deletion-is-logical-bytes-are-reclaimed-by-garbage-collection.md) | Deletion is logical; bytes are reclaimed by garbage collection | Accepted |
| [0019](0019-mcp-lands-in-milestone-3-not-milestone-1.md) | MCP lands in Milestone 3, not Milestone 1 | Accepted |
| [0020](0020-managed-linked-and-vault-assets.md) | Managed, linked and vault assets | Accepted |
| [0021](0021-encrypted-vault-content.md) | Encrypted vault content | Proposed |
| [0022](0022-device-enrolment-and-key-recovery.md) | Device enrolment and key recovery | Accepted |
| [0023](0023-the-external-media-toolchain-is-optional.md) | The external media toolchain is optional, capability-routed and pinned | Accepted |
| [0024](0024-one-consumption-session-model.md) | One ConsumptionSession model for watching, listening and reading | Accepted |
| [0025](0025-external-services-are-optional-and-capability-routed.md) | External network services are optional and capability-routed | Accepted |
| [0026](0026-indexers-are-not-reproducible-so-fixtures-are-the-primary-test-strategy.md) | Indexers are not reproducible, so fixtures are the primary test strategy | Accepted |
| [0027](0027-acquisition-state-is-four-facts-not-one-ordinal.md) | Acquisition state is four facts, not one ordinal | Accepted |
| [0028](0028-discovery-binds-to-torznab-not-prowlarr.md) | Discovery binds to Torznab, the protocol, not Prowlarr, the product | Accepted |
| [0029](0029-a-full-peer-is-controller-attached.md) | A Full Peer is controller-attached and runs no control plane | Superseded by 0038 |
| [0030](0030-replication-is-a-destination-pull.md) | Replication is a destination pull; the controller never carries bytes | Accepted |
| [0031](0031-provider-credentials-are-typed-by-their-auth-scheme.md) | Provider credentials are typed by the provider's declared auth scheme | Accepted |
| [0032](0032-the-personal-mcp-is-device-side-and-device-keys-land-before-they-authorise-anything.md) | The Personal MCP is device-side, and device keys land before they authorise anything | Accepted |
| [0033](0033-a-full-peer-authenticates-to-the-controller-with-its-peer-identity.md) | A Full Peer authenticates to the controller with its ADR-0012 identity | Accepted |
| [0034](0034-a-chunk-manifest-is-an-optimisation-not-an-identity.md) | A chunk manifest is an optimisation, and a blob is never addressed by its chunks | Accepted |
| [0035](0035-a-resumed-transfer-trusts-nothing-it-has-not-re-verified.md) | A resumed transfer trusts nothing it has not re-verified itself | Accepted |
| [0036](0036-integrity-repair-stages-a-whole-replacement.md) | Integrity repair stages a whole replacement; a blob is never edited in place | Accepted |
| [0037](0037-one-way-reachability-is-reported-not-refused.md) | One-way reachability is reported at enrolment, never refused | Accepted |
| [0038](0038-there-is-no-central-authority-peers-are-repositories.md) | Each peer is authoritative for its own site | Accepted |
| [0039](0039-worker-capability-advertisement-is-durable-proven-by-execution-and-expires.md) | Worker capability advertisement is durable, proven by execution, and expires | Accepted |
| [0040](0040-a-renderer-fetches-bytes-with-a-capability-not-a-credential.md) | A renderer fetches bytes with a capability, not a credential | Accepted |
| [0041](0041-a-transfer-session-is-local-and-pieces-are-not-chunks.md) | A TransferSession is local to a peer, and pieces are not chunks | Accepted |
| [0042](0042-piece-exchange-is-native-and-rides-the-peer-surface.md) | Piece exchange is Heyarr's own, and rides the peer surface | Accepted |
| [0043](0043-a-piece-transfer-writes-sparsely-and-its-bitset-is-a-hint.md) | A piece transfer writes sparsely, and its record of what landed is a hint | Accepted |
| [0044](0044-a-controller-backup-is-a-signed-whole-database-snapshot.md) | A controller backup is a signed whole-database snapshot, a restore verifies it, and it carries the controller's identity wrapped | Proposed |
| [0045](0045-a-partial-holding-is-transfer-scoped-and-never-a-replica.md) | A partial holding is transfer-scoped, and is never a replica | Accepted |
| [0046](0046-a-control-plane-backup-is-pushed-to-peers.md) | A control-plane backup is pushed to peers; content is pulled by them | Proposed |
| [0047](0047-a-peer-says-what-it-speaks-and-a-web-seed-is-a-configuration.md) | A peer says what it speaks, and a web seed is a configuration | Accepted |
| [0048](0048-a-cross-site-grant-is-a-signed-expiring-delegation.md) | A cross-site grant is a signed, expiring delegation verified against a pinned key | Proposed |
| [0049](0049-a-space-key-is-wrapped-for-a-device-encryption-key-and-a-peer-cannot-unwrap-it.md) | A space key is wrapped for a device encryption key, and a peer cannot unwrap it | Proposed |
| [0050](0050-external-identifiers-are-readable-for-knowledge-graph-reconciliation.md) | External identifiers are readable, for knowledge-graph reconciliation | Accepted |
| [0051](0051-personal-state-reaches-clients-through-a-device-gateway-not-the-controller.md) | Personal state reaches clients through a device gateway, not the controller | Accepted |
| [0052](0052-a-disposable-download-daemon-earns-a-scheduled-lane.md) | A disposable download-client daemon earns a scheduled acceptance lane (amends 0026) | Accepted |
| [0053](0053-a-weblogin-broker-for-browser-and-tv-qr-login.md) | A weblogin.Broker for browser/TV QR login (and, later, push) | Accepted |
| [0054](0054-client-strategy-first-party-key-holder-compat-adapters-are-reach.md) | Client strategy: a first-party device-side key-holder is the product; compat adapters are reach | Accepted |
| [0055](0055-a-push-login-channel-over-the-voidbind-notify-plane.md) | A push-login channel over the Voidbind notify plane | Accepted |
| [0056](0056-the-item-scope-is-the-sanctioned-addition.md) | The Item entity and the item scope are the sanctioned addition, not a retrofit | Accepted |
| [0057](0057-a-followed-source-projects-items-onto-wants.md) | A followed source projects items onto wants; the follow beat is the search beat's sibling | Accepted |
| [0058](0058-the-feed-provider-is-capability-metadata-tvdb-first.md) | The feed adapter is a CapabilityMetadata provider; TVDB is the first, TMDB is pluggable | Accepted |
| [0059](0059-the-poll-outcome-is-stored-and-a-want-is-created-through-one-path.md) | The poll outcome is stored (not derived), and a want is created through one shared path | Accepted |
| [0060](0060-a-direct-release-acquires-a-non-search-source.md) | A direct release acquires a non-search source (podcast enclosures) | Accepted |
| [0061](0061-a-follow-management-grant-is-the-interim-web-login-write-path.md) | A follow-management grant is the interim web-login write path | Accepted |
| [0062](0062-a-followed-youtube-video-is-acquired-by-a-tagged-subprocess-transport.md) | A followed YouTube video is acquired by a tagged subprocess transport | Accepted |
| [0063](0063-a-followed-article-is-archived-as-a-self-contained-single-file.md) | A followed article is archived as a self-contained single file | Accepted |
| [0064](0064-a-followed-source-is-routed-to-its-adapter-by-type.md) | A followed source is routed to its adapter by type | Accepted |
| [0066](0066-the-node-hosts-the-voidbind-relay-beside-its-legacy-pair-relay.md) | The node hosts the Voidbind relay beside its legacy pair relay | Accepted |
| [0067](0067-a-paired-device-enrols-itself-and-earns-only-the-read-floor.md) | A paired device enrols itself, and earns only the read floor | Accepted |
| [0068](0068-membership-ops-replace-root-only-certs.md) | Membership ops replace root-only certs | Accepted |
| [0070](0070-a-device-refusal-hints-only-at-a-clock.md) | A device refusal hints only at a clock, a proof's life is capped, and a session dies with its approver | Accepted |
