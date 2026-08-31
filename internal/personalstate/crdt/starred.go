package crdt

// starred.go is the client-side, plaintext merge logic for a user's STARRED (or
// favourited) items — the second personal-state CRDT after the playlist (§37,
// §46; issue #386). It feeds the Subsonic `star`/`unstar`/`getStarred2` methods
// and the `getAlbumList2?type=starred` list, all decrypted on the device (never
// on the controller — Invariant 6, §72).
//
// # The model: an add-wins observed-remove set (OR-Set), minus ordering
//
// Starred is exactly the playlist's membership algebra with the positional order
// dropped: starring an item is an OR-Set add under a globally-unique tag,
// unstarring is an observed-remove that tombstones only the tags it saw, and an
// item is starred iff it has a live (non-tombstoned) add-tag. That makes
// concurrent star + unstar of the same item resolve ADD-WINS — the same rule and
// the same convergence proof the playlist carries (§43). Merge is the union of
// the add-tags and the union of the tombstones (both grow-only sets keyed by
// unique tags) and the maximum Lamport counter — a semilattice join, hence
// commutative, associative, and idempotent.
//
// # Ordering getStarred2 without a coordinator
//
// A star does carry ONE scalar: a Lamport `At` counter, used only to order
// `getStarred2` most-recently-starred-first. It plays no part in membership. An
// item's list position is the LARGEST `At` among its live tags (its most recent
// star), and the list is sorted by (At desc, itemID) so two devices agree on the
// order from the change data alone, never from arrival order.

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// StarOp names the kind of change: a star (add) or an unstar (observed-remove).
type StarOp uint8

const (
	// OpStar stars an item under a fresh, unique [StarTag].
	OpStar StarOp = iota
	// OpUnstar tombstones the specific star-tags it observed — and only those.
	OpUnstar
)

// StarTag is the globally unique identity of a SINGLE star operation (a UUIDv7
// string). The OR-Set tracks tags, not item ids: two independent stars of the
// same item are two different tags, so a concurrent star survives an unstar that
// never saw it.
type StarTag string

// StarChange is one star/unstar ready to ship to another device and merge. It is
// self-describing: a star carries its ItemID, its unique Tag and its At counter;
// an unstar carries the exact set of tags it Observed.
type StarChange struct {
	Op StarOp
	// ItemID is the application content id the change concerns (star and unstar).
	ItemID string
	// Tag is this star's unique identity (OpStar only).
	Tag StarTag
	// At is this star's Lamport recency counter (OpStar only).
	At uint64
	// Observed is the set of star-tags this unstar tombstones (OpUnstar only).
	Observed []StarTag
}

// starRecord is the payload stored against a live star-tag.
type starRecord struct {
	itemID string
	at     uint64
}

// lesserStar is the lattice join for two records that landed under the SAME tag.
// A UUIDv7 tag is globally unique, so a tag maps to one record and this is never
// reached legitimately; but a StarChange is unsigned, and an authorised-but-
// malicious device could ship two stars under one tag. A blind overwrite would
// be last-write-by-arrival-order and two replicas would DIVERGE. Choosing the
// deterministically-lesser record by (itemID, at) makes the OR-Set a true
// semilattice again — every replica keeps the same record for the tag no matter
// what order the colliding stars arrive in.
func lesserStar(a, b starRecord) starRecord {
	if a.itemID != b.itemID {
		if a.itemID < b.itemID {
			return a
		}
		return b
	}
	if a.at != b.at {
		if a.at < b.at {
			return a
		}
		return b
	}
	return a
}

// StarSet is the materialised set of starred items: the full OR-Set plus the
// Lamport clock. Every field merges by a lattice join (map/set union, integer
// max), so the whole struct is a semilattice.
type StarSet struct {
	adds       map[StarTag]starRecord
	tombstones map[StarTag]struct{}
	counter    uint64
}

// StarEntry is one starred item: its id and the recency counter it sorts by (the
// largest At among its live star-tags).
type StarEntry struct {
	ID string
	At uint64
}

// NewStarSet returns an empty starred set.
func NewStarSet() *StarSet {
	return &StarSet{
		adds:       make(map[StarTag]starRecord),
		tombstones: make(map[StarTag]struct{}),
	}
}

func newStarTag() StarTag {
	return StarTag(uuid.Must(uuid.NewV7()).String())
}

// Star records a local star of itemID and returns the [StarChange] to ship. The
// At counter takes the next Lamport value (one past the highest this set has
// seen), so a star made after observing others sorts more-recent than them. The
// change is applied locally before it is returned.
func (s *StarSet) Star(itemID string) StarChange {
	// Saturating increment: a malicious change can poison the clock to
	// math.MaxUint64 (applyOne takes the max); a bare ++ there wraps to 0 and
	// every future local star would sort as the OLDEST. Saturating keeps the
	// clock monotonic — once poisoned, new stars share MaxUint64 and settle by
	// their tie-break tag, sorting most-recent rather than jumping to the bottom.
	if s.counter < math.MaxUint64 {
		s.counter++
	}
	c := StarChange{Op: OpStar, ItemID: itemID, Tag: newStarTag(), At: s.counter}
	s.applyOne(c)
	return c
}

// Unstar records a local unstar of itemID and returns the [StarChange] to ship.
// It observes every star-tag currently LIVE for itemID and tombstones exactly
// those; a concurrent star this device has not seen is not observed and survives
// — add-wins in one method.
func (s *StarSet) Unstar(itemID string) StarChange {
	c := StarChange{Op: OpUnstar, ItemID: itemID, Observed: s.liveStarTags(itemID)}
	s.applyOne(c)
	return c
}

// liveStarTags returns the sorted live (added, not tombstoned) tags of itemID.
func (s *StarSet) liveStarTags(itemID string) []StarTag {
	var tags []StarTag
	for tag, rec := range s.adds {
		if rec.itemID != itemID {
			continue
		}
		if _, dead := s.tombstones[tag]; dead {
			continue
		}
		tags = append(tags, tag)
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i] < tags[j] })
	return tags
}

// Apply folds changes into the set. It is the CRDT join over changes: idempotent,
// commutative, and associative — the result never depends on arrival order.
func (s *StarSet) Apply(changes ...StarChange) {
	for _, c := range changes {
		s.applyOne(c)
	}
}

func (s *StarSet) applyOne(c StarChange) {
	switch c.Op {
	case OpStar:
		rec := starRecord{itemID: c.ItemID, at: c.At}
		if existing, ok := s.adds[c.Tag]; ok {
			rec = lesserStar(existing, rec)
		}
		s.adds[c.Tag] = rec
		if c.At > s.counter {
			s.counter = c.At
		}
	case OpUnstar:
		for _, tag := range c.Observed {
			s.tombstones[tag] = struct{}{}
		}
	}
}

// MergeStars joins any number of sets into a NEW set, leaving the inputs
// untouched — the union of every star-tag and tombstone and the maximum Lamport
// counter. Commutative, associative, and idempotent.
func MergeStars(sets ...*StarSet) *StarSet {
	out := NewStarSet()
	for _, s := range sets {
		if s == nil {
			continue
		}
		for tag, rec := range s.adds {
			if existing, ok := out.adds[tag]; ok {
				rec = lesserStar(existing, rec)
			}
			out.adds[tag] = rec
		}
		for tag := range s.tombstones {
			out.tombstones[tag] = struct{}{}
		}
		if s.counter > out.counter {
			out.counter = s.counter
		}
	}
	return out
}

// Clone returns an independent deep copy.
func (s *StarSet) Clone() *StarSet { return MergeStars(s) }

// IsStarred reports whether itemID has any live star-tag.
func (s *StarSet) IsStarred(itemID string) bool {
	for tag, rec := range s.adds {
		if rec.itemID != itemID {
			continue
		}
		if _, dead := s.tombstones[tag]; !dead {
			return true
		}
	}
	return false
}

// Starred returns the starred items, most-recently-starred first.
//
// An item is present iff it has a live star-tag; its sort key is the LARGEST At
// among its live tags (its most recent star), so concurrent stars settle on one
// deterministic recency. Items are then sorted by (At desc, id) — a function of
// the change data only, never of the order changes were applied.
//
// SABOTAGE NOTE (for the reviewer): the recency guard is this sort key. Break it
// by taking the FIRST At seen during Apply, or by recording an incrementing
// apply-sequence and sorting on that instead of At — the SET of starred items
// stays correct, but their order then leaks the application order and
// TestStarredConvergesUnderReordering fails, because two permutations of the same
// changes yield different orders.
func (s *StarSet) Starred() []StarEntry {
	best := make(map[string]uint64)
	for tag, rec := range s.adds {
		if _, dead := s.tombstones[tag]; dead {
			continue
		}
		if cur, seen := best[rec.itemID]; !seen || rec.at > cur {
			best[rec.itemID] = rec.at
		}
	}
	out := make([]StarEntry, 0, len(best))
	for id, at := range best {
		out = append(out, StarEntry{ID: id, At: at})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At > out[j].At // most recent first
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// StarredIDs is Starred reduced to the ordered item ids, the common read.
func (s *StarSet) StarredIDs() []string {
	entries := s.Starred()
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

// Encode is a canonical, deterministic serialisation of the ENTIRE set — every
// star-tag, every tombstone, and the counter — with all sets sorted. Two sets
// that have converged produce byte-identical output. A test and debugging aid,
// not a wire format.
func (s *StarSet) Encode() string {
	var b strings.Builder
	tags := make([]StarTag, 0, len(s.adds))
	for tag := range s.adds {
		tags = append(tags, tag)
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i] < tags[j] })
	b.WriteString("stars:\n")
	for _, tag := range tags {
		rec := s.adds[tag]
		fmt.Fprintf(&b, "  %s=%s@%d\n", tag, rec.itemID, rec.at)
	}
	tombs := make([]StarTag, 0, len(s.tombstones))
	for tag := range s.tombstones {
		tombs = append(tombs, tag)
	}
	sort.Slice(tombs, func(i, j int) bool { return tombs[i] < tombs[j] })
	b.WriteString("tombstones:\n")
	for _, tag := range tombs {
		fmt.Fprintf(&b, "  %s\n", tag)
	}
	fmt.Fprintf(&b, "counter:%d\n", s.counter)
	return b.String()
}
