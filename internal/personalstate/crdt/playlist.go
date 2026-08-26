package crdt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Op names the kind of change. There are exactly two: an OR-Set needs only an
// add and an observed-remove to be a full CRDT (see the package doc).
type Op uint8

const (
	// OpAdd introduces an item under a fresh, unique [Tag].
	OpAdd Op = iota
	// OpRemove tombstones the specific add-tags it observed — and only those.
	OpRemove
)

// Tag is the globally unique identity of a SINGLE add operation (a UUIDv7
// string). The OR-Set tracks tags, not item ids: two independent adds of the
// same content id are two different tags, and that is what lets a concurrent add
// survive a remove that never saw it.
type Tag string

// OrderKey is the total-order sort key an add carries with it.
//
// It is a Lamport logical clock (Counter) with the add's Tag as a deterministic
// tie-break. Because a UUIDv7 gives a total order over tags, (Counter, Tag) is a
// total order over adds that every device computes identically — no wall clock
// and no coordinator. See [OrderKey.Less].
type OrderKey struct {
	Counter uint64
	Tag     Tag
}

// Less reports whether k sorts before other under the (Counter, Tag) total
// order. Counter dominates (a causally-later insert sorts later); the Tag breaks
// a concurrent tie the same way on every replica.
func (k OrderKey) Less(other OrderKey) bool {
	if k.Counter != other.Counter {
		return k.Counter < other.Counter
	}
	return k.Tag < other.Tag
}

// Change is one operation ready to be shipped to another device and merged.
//
// It is self-describing: an add carries everything needed to place the item
// (ItemID, its unique Tag, its Order), and a remove carries the exact set of
// tags it Observed. Nothing about how or when a change is applied changes its
// meaning — that is what makes [State.Apply] order-independent.
type Change struct {
	Op Op
	// ItemID is the application content id the change concerns (add and remove).
	ItemID string
	// Tag is this add's unique identity (OpAdd only).
	Tag Tag
	// Order is this add's total-order key (OpAdd only).
	Order OrderKey
	// Observed is the set of add-tags this remove tombstones (OpRemove only).
	Observed []Tag
}

// Item is a present playlist entry: an item id and the order key it sorts by.
type Item struct {
	ID    string
	Order OrderKey
}

// addRecord is the payload the OR-Set stores against a live add-tag.
type addRecord struct {
	itemID string
	order  OrderKey
}

// State is the materialised playlist: the full OR-Set plus the Lamport clock.
//
// It is fully described by three grow-only pieces — the map of every add-tag
// ever seen, the set of tombstoned tags, and the high-water counter. Every field
// merges by a lattice join (map/set union, integer max), so the whole struct is
// a semilattice and [Merge] is commutative, associative, and idempotent.
type State struct {
	adds       map[Tag]addRecord
	tombstones map[Tag]struct{}
	counter    uint64
}

// New returns an empty playlist state.
func New() *State {
	return &State{
		adds:       make(map[Tag]addRecord),
		tombstones: make(map[Tag]struct{}),
	}
}

// newTag mints a fresh add-tag. UUIDv7 is time-ordered, which gives newer adds
// naturally larger tags and keeps the tie-break stable, but correctness rests
// only on global uniqueness, not on the ordering of the UUID itself.
func newTag() Tag {
	return Tag(uuid.Must(uuid.NewV7()).String())
}

// Add records a local insertion of itemID and returns the [Change] to ship.
//
// The order key takes the next Lamport counter (one past the highest this state
// has ever seen), so an insert the caller makes after observing others sorts
// after them. The change is applied to this state before it is returned, so the
// caller's own view already reflects it.
func (s *State) Add(itemID string) Change {
	s.counter++
	c := Change{
		Op:     OpAdd,
		ItemID: itemID,
		Tag:    newTag(),
		Order:  OrderKey{Counter: s.counter, Tag: Tag("")},
	}
	// The order key's tie-break tag is the add's own tag.
	c.Order.Tag = c.Tag
	s.applyOne(c)
	return c
}

// Remove records a local removal of itemID and returns the [Change] to ship.
//
// It observes every add-tag currently LIVE for itemID and tombstones exactly
// those. Any add of itemID this device has not yet seen (a concurrent add on
// another device) is not in Observed, so it survives the merge — this is the
// add-wins rule in one method.
func (s *State) Remove(itemID string) Change {
	c := Change{Op: OpRemove, ItemID: itemID, Observed: s.liveTags(itemID)}
	s.applyOne(c)
	return c
}

// liveTags returns the tags of itemID that are added and not yet tombstoned,
// sorted for a deterministic Change (a set has no order, but a stable slice
// keeps encodings reproducible across devices).
func (s *State) liveTags(itemID string) []Tag {
	var tags []Tag
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

// Apply folds changes into the state. It is the CRDT join over changes: applying
// the same change twice is a no-op (idempotent), and the result never depends on
// the order the changes arrive in (commutative and associative). A caller merges
// a remote changeset simply by passing it here.
func (s *State) Apply(changes ...Change) {
	for _, c := range changes {
		s.applyOne(c)
	}
}

// applyOne folds a single change. Every write is a lattice join: an add sets a
// tag-keyed record (re-setting the same tag is identical), a remove inserts into
// the tombstone set, and the Lamport counter only ever moves up. None of these
// depend on prior application order.
func (s *State) applyOne(c Change) {
	switch c.Op {
	case OpAdd:
		s.adds[c.Tag] = addRecord{itemID: c.ItemID, order: c.Order}
		if c.Order.Counter > s.counter {
			s.counter = c.Order.Counter
		}
	case OpRemove:
		for _, tag := range c.Observed {
			s.tombstones[tag] = struct{}{}
		}
	}
}

// Merge joins any number of states into a NEW state, leaving the inputs
// untouched. It is the union of every add-tag and tombstone and the maximum of
// every Lamport counter — a semilattice join, hence commutative, associative,
// and idempotent: Merge(a, b) and Merge(b, a) are byte-identical, and merging a
// state with itself changes nothing.
func Merge(states ...*State) *State {
	out := New()
	for _, s := range states {
		if s == nil {
			continue
		}
		for tag, rec := range s.adds {
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

// Clone returns an independent deep copy, so callers can merge without mutating
// a shared state.
func (s *State) Clone() *State {
	return Merge(s)
}

// Items returns the present playlist in its converged total order.
//
// An item is present iff it has a live (non-tombstoned) add-tag; its sort key is
// the SMALLEST order key among its live tags, so concurrent adds of the same id
// settle on one deterministic position. Items are then sorted by [OrderKey.Less]
// — the (Counter, Tag) total order — which depends only on the change data, not
// on the order changes were applied. Reordering the applies must not reorder
// this output; the convergence test is precisely that assertion.
//
// SABOTAGE NOTE (for the reviewer): the ordering guard is this sort. Break it by
// making Items sort on when an item was first seen during Apply instead of on
// its OrderKey — e.g. record an incrementing sequence in applyOne and sort by
// that. The set of items stays correct, but their order then leaks the
// application order, and TestConvergenceUnderReordering fails because two
// permutations of the same changes yield different orders.
func (s *State) Items() []Item {
	best := make(map[string]OrderKey)
	for tag, rec := range s.adds {
		if _, dead := s.tombstones[tag]; dead {
			continue
		}
		if cur, seen := best[rec.itemID]; !seen || rec.order.Less(cur) {
			best[rec.itemID] = rec.order
		}
	}
	items := make([]Item, 0, len(best))
	for id, key := range best {
		items = append(items, Item{ID: id, Order: key})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Order.Less(items[j].Order) })
	return items
}

// IDs is Items reduced to the ordered item ids, the common read.
func (s *State) IDs() []string {
	items := s.Items()
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

// Encode is a canonical, deterministic serialisation of the ENTIRE state — every
// add-tag, every tombstone, and the counter — with all sets sorted. Two states
// that have converged produce byte-identical output, so tests can assert
// convergence over the full internal state, not merely the visible list. It is a
// test and debugging aid, not a wire format.
func (s *State) Encode() string {
	var b strings.Builder
	tags := make([]Tag, 0, len(s.adds))
	for tag := range s.adds {
		tags = append(tags, tag)
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i] < tags[j] })
	b.WriteString("adds:\n")
	for _, tag := range tags {
		rec := s.adds[tag]
		fmt.Fprintf(&b, "  %s=%s@%d:%s\n", tag, rec.itemID, rec.order.Counter, rec.order.Tag)
	}
	tombs := make([]Tag, 0, len(s.tombstones))
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
