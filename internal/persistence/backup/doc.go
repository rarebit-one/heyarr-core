// Package backup takes and restores whole-database snapshots of a peer's own
// control plane (spec §49, ADR-0044). Milestone 7.
//
// # What this delivers, and what it does not
//
// It delivers the stream on ONE node, before any peer is involved: taking a
// backup of the live control database, and a restore that OPENS one — verifies
// it, proves it is internally consistent, and refuses it when it is not what it
// claims. Replicating a backup to trusted peers is §50/M7-03; the disaster
// recovery verb that rebuilds a node from a peer's held copy is §51/M7-04. This
// package is what both of those carry.
//
// # The central hazard: a populated -wal is a silently stale backup
//
// [sqlite.DB.Close] already names it — "a populated -wal beside a copied
// database file is a silently stale backup rather than a loud failure." A
// backup taken by copying the file while the node runs, without dealing with
// the WAL, restores to an OLDER database than the one it was taken from and
// looks perfect. So a backup is never a file copy. It is `VACUUM INTO`
// (ADR-0044 question 1): SQLite reads a transactionally consistent snapshot and
// writes a self-contained, defragmented single file with no -wal sidecar —
// there is no sidecar to forget. It is plain SQL, so it exists in the pure-Go
// driver (ADR-0004) rather than needing the C Online Backup API.
//
// # The artefact carries its own provenance, and a restore trusts a signature
//
// Every backup records which peer's control plane it is, a monotonic
// generation (the event-log high-water mark — invariant 7 guarantees one, so
// the generation tracks actual progress and a backup taken when nothing changed
// does not advance it), the schema version, and when the database was read
// ([Manifest]). The shape is [catalog.Meta]'s, for the same reason: an answer
// about a moment that cannot say which moment is worse than no answer.
//
// A restore trusts a SIGNATURE over that provenance, not the party that handed
// the file over (ADR-0044 question 2, the same argument ADR-0043 makes for
// piece hashes). The manifest is signed by the origin peer's Ed25519 identity
// key (ADR-0012); a holder verifies with the public key it already pins.
// Signing is asymmetric, so a holder never needs the private key.
//
// # A backup is not a live control plane
//
// Invariant 5 / ADR-0003: never active-active SQLite. A holder must be unable
// to open a backup as a control plane. [Open] opens it mode=ro + query_only, so
// a write fails at the STORAGE layer with SQLITE_READONLY — the same mechanism
// [catalog.OpenReadOnly] rests on, and not because no code currently attempts a
// write. The only thing that opens a backup as a live writable control database
// is [Restore], which installs it at a DIFFERENT data directory by construction
// — a new node, not the holder.
package backup
