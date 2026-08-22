// Package catalog maintains a Full Peer's local materialised read snapshot for
// degraded operation (spec §52). Milestone 4.
//
// # What this package delivers, and what it deliberately does not
//
// It delivers the ARTIFACT: a snapshot of the controller's catalogue, held on
// the peer, complete enough to answer read questions, fresh enough to be worth
// answering with, and honest about both. It does not serve reads from it, does
// not implement degraded operation, does not cache access leases (§54) and
// does not index anything for search. Those are Milestone 7 and they live in
// internal/peer/degraded, which says so in its own doc comment. §84 puts
// "catalog snapshots" in M4 and "degraded local-read mode" in M7, and the
// split is deliberate: the artifact has to exist and be trustworthy before
// anything is allowed to answer with it.
//
// # The constraint is a mechanism, not a comment
//
// §52: "The snapshot should not be treated as independently writable control
// state." That sentence will be respected for exactly as long as nobody is in
// a hurry, so it is enforced structurally:
//
//   - The snapshot lives in its OWN DATABASE FILE, not as a shadow schema
//     inside the peer's control database. A shadow schema is one careless
//     UPDATE away from a second writer against the control plane, and
//     Invariant 5 / ADR-0003 is the invariant that costs the most to violate.
//   - The only writer is [Store.Apply], reached through a handle obtained from
//     [Open]. Everything that merely reads obtains its handle from
//     [OpenReadOnly], which opens the file with SQLite's mode=ro and
//     query_only — so a write through the read path fails at the STORAGE
//     layer, with SQLITE_READONLY, and not because no code currently attempts
//     one.
//   - [Open] refuses a path that already holds a control database, because the
//     cheapest way to end up with two writers on the control plane is to point
//     the snapshot builder at it.
//
// # Honest staleness
//
// A snapshot is a fact about a moment. Every one carries the identity of the
// controller it came from, a monotonic version and the instant the catalogue
// was read ([Metadata]), and applying a version that does not advance is
// refused ([ErrStaleSnapshot]). §53 asks the system to become "conservative
// rather than unavailable" — which only works if whatever serves the
// conservative answer can say how old it is. A stale answer presented as
// current is worse than an unavailable one.
//
// A peer that has never built a snapshot reports [ErrNoSnapshot], never an
// empty one. In M7 "the library is empty" and "I cannot help you" are
// different sentences, and conflating them is a bug that surfaces at the worst
// possible moment.
//
// # Contents
//
// Libraries, library roots, works, editions, blobs and assets — library
// organisation, the semantic spine, basic metadata and the blob mappings that
// make bytes locatable. Artwork references and known device access leases are
// listed by §52 as "potential contents" and are out of scope here: leases are
// §54/M7 and there is no lease model to snapshot yet.
package catalog
