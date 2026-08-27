// Package replication reconciles encrypted personal state to trusted Full Peers
// (§37, §45, §46, ADR-0038, ADR-0049). Personal state is small and less
// replaceable than media, so its placement default is EVERY trusted Full Peer:
// a device syncs against whichever site it can reach, and every Full Peer holds a
// full replica of every space's metadata, wrapped keys and changes — as
// ciphertext none of them can read (Invariant 6).
//
// It is leaderless and local-first (§46): this node offers each Full Peer what it
// is missing and pushes only that, over the state-sync protocol on the peer
// surface (internal/api/peerapi). A failed sync is Tuesday, not a fault
// (ADR-0038): an unreachable peer is a recorded fact with a timestamp, and the
// next cycle retries. Re-running once converged is a no-op (Invariant 9).
//
// Like the peer surface, this package moves OPAQUE changes and NEVER decrypts
// one: it depends on internal/personalstate/protocol (the opaque wire change) and
// the store's opaque rows, not on the plaintext CRDT model — the same boundary
// the peer surface holds (a depguard rule and boundary test), for the same reason.
package replication
