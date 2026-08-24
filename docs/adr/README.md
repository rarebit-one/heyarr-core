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
| [0022](0022-device-enrolment-and-key-recovery.md) | Device enrolment and key recovery | Proposed |
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
| [0038](0038-there-is-no-central-authority-peers-are-repositories.md) | Each peer is authoritative for its own site | Proposed |
| [0039](0039-worker-capability-advertisement-is-durable-proven-by-execution-and-expires.md) | Worker capability advertisement is durable, proven by execution, and expires | Accepted |
| [0040](0040-a-renderer-fetches-bytes-with-a-capability-not-a-credential.md) | A renderer fetches bytes with a capability, not a credential | Accepted |
| [0041](0041-a-transfer-session-is-local-and-pieces-are-not-chunks.md) | A TransferSession is local to a peer, and pieces are not chunks | Proposed |
