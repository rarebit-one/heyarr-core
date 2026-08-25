// Package transfer is §24's shared abstraction over acquisition and
// replication: what is wanted, where it might come from, and in what order to
// try (M6, ADR-0041).
//
// # What it is for
//
// Before M6 the two halves moved bytes by different routes that had nothing in
// common. Replication walked a ranked list of peer sources inside
// ReplicateBlobHandler; acquisition handed a magnet to an external download
// client and waited. §23 asks for those to converge — both peers pulling from
// external sources while exchanging with each other — and §24 names the shared
// abstraction that makes it expressible.
//
// A Session is that value: a target, a set of sources of DIFFERENT KINDS, and
// an urgency that says how hard to try. Ordering the attempts is the whole of
// what this package does, and it is pure — no network, no disk, no catalogue.
// The transports are adapters above it.
//
// # A session is LOCAL, and there is no shared session object
//
// ADR-0041. Two peers acquiring the same blob are two sessions that found each
// other, not one session with two members: under ADR-0038 there is no node
// whose loss is special, so there is nothing for a shared object to live on.
// What the two share is the target digest, which is the only identity there is
// (invariant 1).
//
// The rule that falls out, and the one this package exists to make hard to
// break: **a session makes progress with whoever it has.** Never a quorum,
// never a block on an unreachable participant, and completion is always "the
// target digest verified locally" — never a function of who is reachable.
//
// # What this package deliberately does NOT do
//
// It does not move bytes, hold connections, or know what a piece is. It does
// not run sources CONCURRENTLY — §23's simultaneous exchange needs a transport
// with piece-level control (§25), which is not built. Today every transport
// tries sources in order and stops at the first success, which is what
// replication already did; this package is what lets acquisition's sources and
// a web seed sit in that same list.
//
// Nothing here imports os, path/filepath, database/sql, persistence or the CAS
// — depguard enforces it (§18, ADR-0006/0007).
package transfer
