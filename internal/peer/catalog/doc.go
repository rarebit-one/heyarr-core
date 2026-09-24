// Package catalog defines the catalog snapshot wire format (spec §52): the
// payload a node serves on the peer surface's /catalog/snapshot route, and the
// metadata that makes it honest about where it came from and how old it is.
//
// # What is here, and what is not
//
// Only the wire types — [Snapshot], [Meta], the per-table row types, [KindFull]
// and [KindIncremental] — and the canonical [Snapshot.ContentDigest]. The
// builder is internal/persistence/catalog (BuildSnapshot, which also records
// what it issued in peer_snapshots), and the route is internal/api/peerapi.
//
// A peer-side materialised store (a separate read-only SQLite file, a writer
// that applied full and incremental payloads, and a refresher that pulled them
// over the pinned link) used to live here. It was written for Milestone 7's
// degraded local-read mode against the controller-attached model of ADR-0029.
// ADR-0038 superseded that model — each peer is authoritative for its own site,
// so a partitioned peer already browses its own catalogue — and M7-06 (#286)
// was closed as not planned. With nothing ever wiring the store in, it was
// removed. If the store is ever wanted again (ADR-0073 leaves the §52 snapshot's
// future open), it is in git history.
//
// # Honest staleness
//
// A snapshot is a fact about a moment. Every one carries the identity of the
// node it came from, a monotonic version and the instant the catalogue was read
// ([Meta]). An incremental payload also carries the complete id set of every
// covered table, so that deletions are derivable and an incremental refresh and
// a full rebuild of the same catalogue converge on identical contents (see
// [Snapshot]).
package catalog
