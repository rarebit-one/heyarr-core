// Package scenario holds end-to-end tests that compose the whole encrypted
// personal-state plane — the device client, the CRDT, the opaque change protocol,
// the peer store and multi-peer replication — into the scenarios Milestone 9 owes
// as its payoff: two devices converging after an offline concurrent edit, merged
// client-side, with the server holding only ciphertext throughout (§42, §43,
// ADR-0049, #324).
//
// It lives in its own package because it deliberately imports BOTH the
// plaintext-reading device side (client, crdt, statesync, encryption) and the
// opaque peer side (store, replication) — the two halves the depguard boundary
// keeps apart everywhere else. Here they meet only as a test driving the real
// APIs against each other, never as production code crossing the line.
package scenario
