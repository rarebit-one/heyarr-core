// Package crdt is the client-side, plaintext merge logic for a personal
// playlist — an ordered set of items — that Milestone 9 converges after a
// device decrypts concurrent offline edits (spec §42, §43; issue #324).
//
// # What this package is, and what it deliberately is not
//
// This is PURE merge logic. It operates on plaintext, in memory, on the client,
// AFTER changes have been decrypted. It has nothing to do with encryption,
// networking, or persistence, and imports none of them — those live in other
// Milestone 9 tracks and call into this one. Keeping the convergence proof in a
// package with no I/O is the point: the property the milestone rests on is a
// property of an algebra, and an algebra is easiest to trust when it cannot
// touch a disk or a socket.
//
// # The one property that defines correctness (§43)
//
// The merge is a JOIN on a semilattice: COMMUTATIVE, ASSOCIATIVE, and
// IDEMPOTENT. Applying a set of changes in ANY order, with any duplicates,
// yields the SAME converged state. That is exactly what makes two devices that
// edited the same playlist offline converge once they reconnect and exchange
// changes — no coordinator, no last-writer-wins clock, no merge conflict to
// resolve by hand. A merge that is order-dependent has NOT delivered the
// milestone, so the tests treat convergence-under-reordering as the headline
// assertion, not an edge case.
//
// # The model: an add-wins observed-remove set (OR-Set)
//
// A playlist is an ordered set of items. Membership is an OR-Set:
//
//   - Every ADD of an item mints a globally unique TAG (a UUIDv7). The tag — not
//     the item id — is the thing the set actually tracks, so two independent adds
//     of the same content id are two distinct tags.
//   - Every REMOVE records the set of tags it OBSERVED at the moment it ran. A
//     remove tombstones exactly those tags and no others.
//   - An item is PRESENT iff it has at least one add-tag that no remove has
//     tombstoned.
//
// This is ADD-WINS: a remove can only cancel adds it actually saw, so an add
// that happened concurrently on another device (a tag the remove never observed)
// survives. Concurrent add + remove of the same item therefore leaves it
// PRESENT. Merge is the union of the add-tags and the union of the tombstones —
// both are grow-only sets keyed by unique tags, and set union is trivially
// commutative, associative, and idempotent, which is where the whole guarantee
// comes from.
//
// # Ordering without a coordinator
//
// Membership convergence is not enough; two devices must also agree on the ORDER
// of the items. Each add carries an [OrderKey] = a Lamport logical counter plus
// the add's tag as a deterministic tie-break. A device minting a new add sets
// its counter to one past the highest counter it has ever seen (the Lamport
// rule), so a later insert sorts after an earlier one it was aware of. Two
// devices that concurrently insert can land on the SAME counter; the tag
// (unique, total order over UUIDs) breaks the tie identically on both sides. The
// total order is: sort present items by (counter, tag). No wall clock, no
// coordinator, and — critically — the order is a function of the change data,
// never of the order in which changes happened to be applied.
//
// # Scope of this first cut
//
// Add an item, remove an item, list items in order, merge. There is NO
// move/reorder operation yet: reordering an existing item converges as a
// remove-then-add, but a first-class move that preserves identity across the
// reorder (and its own concurrent-move conflict rules) is a deliberate
// follow-up, not part of this package's first cut.
package crdt
