package crdt

// readingpos.go is the client-side, plaintext merge logic for a reader's
// READING-POSITION per publication — the third personal-state CRDT after the
// playlist and the starred set (§37, §45; issue #386). It feeds the OPDS
// reading-position surface, decrypted on the device (Invariant 6, §72).
//
// # The model: a per-publication last-writer-wins (LWW) register
//
// Unlike the playlist and starred sets — which are OR-Sets because a member's
// presence is the thing that must survive a concurrent change — a reading
// position is a single SCALAR per publication, and the intent of two concurrent
// updates is "wherever I read to LAST", not "keep both". So the lattice is a
// per-key LWW register: a map from publication id to the record with the greatest
// [posKey] anyone has written, and the whole map merges elementwise by that max.
//
// A max-register is a semilattice: elementwise maximum is commutative,
// associative, and idempotent, so Apply and [MergeReadingPositions] converge to a
// byte-identical map regardless of arrival order (§43) — the same headline
// property the OR-Sets carry, reached by a different lattice.
//
// # Ordering without a coordinator
//
// The winner is the greatest [posKey] = (At, Writer), a total order every device
// computes identically: a Lamport `At` counter dominates (a write made after
// observing another sorts later), and a globally-unique `Writer` tag (a UUIDv7
// minted per write) breaks a concurrent tie the same way on every replica. A
// hostile colliding write (same At AND Writer, which a real UUIDv7 never
// produces) is further broken by the position string itself, so the join stays a
// total order and two replicas never diverge.

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// PosTag is the globally unique identity of a single position write (a UUIDv7),
// used only as the deterministic tie-break in the [posKey] total order.
type PosTag string

// posKey is the total-order key a write carries: a Lamport counter with the
// write's PosTag as tie-break. (At, Writer) is a total order every device
// computes identically — no wall clock, no coordinator.
type posKey struct {
	At     uint64
	Writer PosTag
}

// greater reports whether k sorts AFTER other under (At, Writer) — the direction
// a max-register joins on (the later write wins).
func (k posKey) greater(other posKey) bool {
	if k.At != other.At {
		return k.At > other.At
	}
	return k.Writer > other.Writer
}

// PositionChange is one reading-position write ready to ship and merge. It is
// self-describing: the publication it concerns, the position, and the total-order
// key (At + Writer) that decides which of two concurrent writes wins.
type PositionChange struct {
	// PubID is the publication whose position this write sets.
	PubID string
	// Position is the opaque locator (an OPDS/EPUB CFI, a percentage, a page) —
	// the personal-state plane never interprets it.
	Position string
	// At is this write's Lamport counter.
	At uint64
	// Writer is this write's unique tie-break identity.
	Writer PosTag
}

// posRecord is the payload stored against a publication id.
type posRecord struct {
	position string
	key      posKey
}

// laterRecord is the lattice join for two writes to the SAME publication: the one
// with the greater posKey wins. A fully-tied key (same At AND Writer — impossible
// from a real UUIDv7, craftable by a malicious device) is broken by the position
// string, so the join is a total order and every replica settles identically.
func laterRecord(a, b posRecord) posRecord {
	if a.key.greater(b.key) {
		return a
	}
	if b.key.greater(a.key) {
		return b
	}
	// Keys are equal: deterministic tie-break on the position value itself.
	if a.position >= b.position {
		return a
	}
	return b
}

// ReadingPositions is the materialised per-publication position map plus the
// Lamport clock. The map merges elementwise by [laterRecord] and the counter by
// max, so the whole struct is a semilattice.
type ReadingPositions struct {
	positions map[string]posRecord
	counter   uint64
}

// PositionEntry is one publication's current position and the Lamport counter of
// the write that set it.
type PositionEntry struct {
	PubID    string
	Position string
	At       uint64
}

// NewReadingPositions returns an empty position map.
func NewReadingPositions() *ReadingPositions {
	return &ReadingPositions{positions: make(map[string]posRecord)}
}

func newPosTag() PosTag {
	return PosTag(uuid.Must(uuid.NewV7()).String())
}

// Set records a local reading-position write for pubID and returns the
// [PositionChange] to ship. The At counter takes the next Lamport value (one past
// the highest this map has seen), so a write made after observing others wins
// over them. The change is applied locally before it is returned.
func (r *ReadingPositions) Set(pubID, position string) PositionChange {
	// Saturating increment: a malicious change can poison the clock to
	// math.MaxUint64 (applyOne takes the max); a bare ++ there wraps to 0 and the
	// next local write would sort as the OLDEST and be unable to update anything.
	// Saturating keeps the clock monotonic — once poisoned, new writes share
	// MaxUint64 and settle by their tie-break Writer.
	if r.counter < math.MaxUint64 {
		r.counter++
	}
	c := PositionChange{PubID: pubID, Position: position, At: r.counter, Writer: newPosTag()}
	r.applyOne(c)
	return c
}

// Apply folds changes into the map. It is the CRDT join over writes: idempotent,
// commutative, and associative — the result never depends on arrival order.
func (r *ReadingPositions) Apply(changes ...PositionChange) {
	for _, c := range changes {
		r.applyOne(c)
	}
}

func (r *ReadingPositions) applyOne(c PositionChange) {
	rec := posRecord{position: c.Position, key: posKey{At: c.At, Writer: c.Writer}}
	if existing, ok := r.positions[c.PubID]; ok {
		rec = laterRecord(existing, rec)
	}
	r.positions[c.PubID] = rec
	if c.At > r.counter {
		r.counter = c.At
	}
}

// MergeReadingPositions joins any number of maps into a NEW map, leaving the
// inputs untouched — the elementwise max per publication and the maximum Lamport
// counter. Commutative, associative, and idempotent.
func MergeReadingPositions(maps ...*ReadingPositions) *ReadingPositions {
	out := NewReadingPositions()
	for _, r := range maps {
		if r == nil {
			continue
		}
		for pub, rec := range r.positions {
			if existing, ok := out.positions[pub]; ok {
				rec = laterRecord(existing, rec)
			}
			out.positions[pub] = rec
		}
		if r.counter > out.counter {
			out.counter = r.counter
		}
	}
	return out
}

// Clone returns an independent deep copy.
func (r *ReadingPositions) Clone() *ReadingPositions { return MergeReadingPositions(r) }

// Position returns the current reading position for pubID, if any.
func (r *ReadingPositions) Position(pubID string) (string, bool) {
	rec, ok := r.positions[pubID]
	if !ok {
		return "", false
	}
	return rec.position, true
}

// All returns every publication's current position, sorted by publication id for
// a deterministic read.
//
// SABOTAGE NOTE (for the reviewer): the winner of two concurrent writes is chosen
// by [laterRecord] on the (At, Writer) total order. Break it by having applyOne
// keep whichever write arrived LAST (a blind overwrite) instead of the greater
// key — a single map still results, but its contents then leak the application
// order and TestReadingConvergesUnderReordering fails, because two permutations
// of the same writes settle on different positions.
func (r *ReadingPositions) All() []PositionEntry {
	out := make([]PositionEntry, 0, len(r.positions))
	for pub, rec := range r.positions {
		out = append(out, PositionEntry{PubID: pub, Position: rec.position, At: rec.key.At})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PubID < out[j].PubID })
	return out
}

// Encode is a canonical, deterministic serialisation of the ENTIRE map — every
// publication's position, order key, and the counter — sorted by publication id.
// Two maps that have converged produce byte-identical output. A test and
// debugging aid, not a wire format.
func (r *ReadingPositions) Encode() string {
	var b strings.Builder
	pubs := make([]string, 0, len(r.positions))
	for pub := range r.positions {
		pubs = append(pubs, pub)
	}
	sort.Strings(pubs)
	b.WriteString("positions:\n")
	for _, pub := range pubs {
		rec := r.positions[pub]
		fmt.Fprintf(&b, "  %s=%q@%d:%s\n", pub, rec.position, rec.key.At, rec.key.Writer)
	}
	fmt.Fprintf(&b, "counter:%d\n", r.counter)
	return b.String()
}
