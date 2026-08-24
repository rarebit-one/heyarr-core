// Package pieces is the internal transport: fixed-length pieces of a blob,
// exchanged between enrolled peers over the authenticated peer surface
// (spec §25, §26, §27, ADR-0041, ADR-0042). Milestone 6.
//
// # It is not a BitTorrent client, and the name says so
//
// This directory was `torrent` from Milestone 1 until ADR-0042, which chose
// Heyarr's own piece exchange over linking an engine. The reasoning is on the
// ADR and the short form is that the internal transport never speaks to a
// non-Heyarr peer: §25 keeps external acquisition with Transmission, and §26
// takes peer discovery from authenticated membership rather than a tracker. So
// BitTorrent wire compatibility is a cost with no counterparty.
//
// Renamed rather than filled, because a package called `torrent` that speaks no
// BitTorrent is a name that will mislead every reader after the first.
//
// # What a piece is
//
// A byte range of the target blob, fixed length and aligned from zero, its
// geometry derived deterministically from the blob so two peers compute the
// same one without agreeing on anything.
//
// ADR-0041: a piece is NOT a chunk. Chunks are content-defined (FastCDC) and
// exist for dedup, reuse, resume and repair; they are durable and live in a
// manifest. Pieces are fixed, exist only for the duration of a session, and are
// never an identity. Making chunks fixed-size to match pieces would destroy the
// dedup those content-defined boundaries exist for, and would present as
// "replication moves more bytes than it used to" with nothing red.
//
// # Verification is two-level and invariant 1 is not delegated
//
// A received piece is checked against its own hash during transfer, which is
// what makes exchange safe against a peer that lies. The WHOLE OBJECT is then
// checked against its BLAKE3 digest before it becomes a blob — invariant 1,
// §21, ADR-0005 — and that check is not skipped because the pieces passed.
package pieces
