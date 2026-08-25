// Package backupsync distributes a peer's control-plane backup to the peers
// that trust it, and holds the backups pushed to this one (spec §50, ADR-0046).
// Milestone 7.
//
// # Two halves, one surface
//
// The SENDER half is a controller pushing its own signed backup to every
// trusted Full Peer over the mTLS peer surface. Push, not pull (ADR-0046): the
// bytes are the controller's own small state, so ADR-0030's "never carry
// content bytes" does not apply, and pushing moves the backup structurally
// opposite the pulled catalog snapshot — one more way the two artefacts cannot
// be confused (§50).
//
// The RECEIVER half is [Store]: a peer holds the backups pushed to it, keyed by
// the SOURCE peer's id and the generation, each one inert. It is verified
// before it is stored — signature against the source's pinned key, digest
// against the bytes — and it is never opened as a control plane. That refusal
// is [backup.Open]'s, reused: a held backup opens mode=ro + query_only, so a
// write fails at the storage layer (invariant 5, ADR-0044 Q5).
//
// # It is not the catalog snapshot
//
// §50 is emphatic that the controller backup is distinct from the peer's
// read-oriented catalog snapshot (§52), and they are told apart here by
// everything: a different directory (received control backups vs
// catalog-snapshot.db), a different transport direction (pushed here, the
// snapshot pulled), a different content (the whole control plane vs a read
// view), and the `goose_db_version` + `peers` marker a control database carries
// and a snapshot does not.
//
// # A belief and a fact
//
// The sender records what generation it believes each peer holds, so it can
// answer "peer B is a generation behind" even when B is unreachable and cannot
// be asked. The receiver holds the fact — the generations actually on its disk.
// When the two disagree the receiver's answer wins, the same lesson
// internal/peer/durability learned in M4: a controller's belief about a machine
// it is not is a belief.
package backupsync
