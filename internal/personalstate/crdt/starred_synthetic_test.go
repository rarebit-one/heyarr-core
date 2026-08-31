package crdt

import (
	"math"
	"math/rand"
	"testing"
)

// Adversarial synthetic tests for the starred OR-Set: hostile StarChange values
// an authorised-but-malicious device could craft (§42) — an unstar observing
// tags never starred, a colliding tag, a Lamport counter at the ceiling. Every
// replica must still converge to the same set regardless of arrival order (§43),
// degrade predictably, or have its boundary pinned here.

func applyStarsIn(changes []StarChange) *StarSet {
	s := NewStarSet()
	s.Apply(changes...)
	return s
}

func shuffledStars(changes []StarChange, seed int64) []StarChange {
	out := append([]StarChange(nil), changes...)
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test shuffle
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestStarredConvergesUnderHostileOrdering: arbitrary stars, unstars of live
// tags, and unstars of tags never starred, applied in 200 random orders, all
// converge to one byte-identical set.
func TestStarredConvergesUnderHostileOrdering(t *testing.T) {
	t.Parallel()
	var changes []StarChange
	tags := make([]StarTag, 0, 20)
	for i := 0; i < 20; i++ {
		tag := StarTag(string(rune('a'+i)) + "-tag")
		tags = append(tags, tag)
		changes = append(changes, StarChange{
			Op: OpStar, ItemID: "item-" + string(rune('a'+(i%5))), Tag: tag, At: uint64(i % 7),
		})
	}
	changes = append(changes, StarChange{Op: OpUnstar, ItemID: "item-a", Observed: []StarTag{tags[0], tags[5]}})
	changes = append(changes, StarChange{Op: OpUnstar, ItemID: "ghost", Observed: []StarTag{"never-1", "never-2"}})

	want := applyStarsIn(changes).Encode()
	for seed := int64(0); seed < 200; seed++ {
		if got := applyStarsIn(shuffledStars(changes, seed)).Encode(); got != want {
			t.Fatalf("permutation seed=%d diverged:\n got=%s\nwant=%s", seed, got, want)
		}
	}
}

// TestStarredGhostUnstarIsHarmless: unstarring a tag with no star is inert and
// converges whichever side of an unrelated star it arrives.
func TestStarredGhostUnstarIsHarmless(t *testing.T) {
	t.Parallel()
	star := StarChange{Op: OpStar, ItemID: "x", Tag: "real", At: 1}
	ghost := StarChange{Op: OpUnstar, ItemID: "x", Observed: []StarTag{"phantom"}}
	a := applyStarsIn([]StarChange{star, ghost})
	b := applyStarsIn([]StarChange{ghost, star})
	if a.Encode() != b.Encode() {
		t.Fatal("a ghost tombstone made the result order-dependent")
	}
	if !a.IsStarred("x") {
		t.Fatal("the real star did not survive a ghost tombstone")
	}
}

// TestStarredPreemptiveTombstoneConverges: an unstar observing a tag BEFORE its
// star arrives converges the same as star-then-unstar, and the tag ends dead.
func TestStarredPreemptiveTombstoneConverges(t *testing.T) {
	t.Parallel()
	star := StarChange{Op: OpStar, ItemID: "x", Tag: "t1", At: 1}
	unstar := StarChange{Op: OpUnstar, ItemID: "x", Observed: []StarTag{"t1"}}
	if applyStarsIn([]StarChange{star, unstar}).Encode() != applyStarsIn([]StarChange{unstar, star}).Encode() {
		t.Fatal("pre-emptive tombstone diverged from star-then-unstar")
	}
	if applyStarsIn([]StarChange{unstar, star}).IsStarred("x") {
		t.Fatal("the tombstoned tag survived")
	}
}

// TestStarredReplayIsIdempotent: replaying a changeset any number of times equals
// applying it once.
func TestStarredReplayIsIdempotent(t *testing.T) {
	t.Parallel()
	changes := []StarChange{
		{Op: OpStar, ItemID: "a", Tag: "ta", At: 1},
		{Op: OpStar, ItemID: "b", Tag: "tb", At: 2},
		{Op: OpUnstar, ItemID: "a", Observed: []StarTag{"ta"}},
	}
	once := applyStarsIn(changes).Encode()
	many := NewStarSet()
	for i := 0; i < 50; i++ {
		many.Apply(shuffledStars(changes, int64(i))...)
	}
	if many.Encode() != once {
		t.Fatal("replaying stars changed the set: not idempotent")
	}
}

// TestStarredCollidingTagsConverge: two stars under the SAME tag with different
// payloads — impossible from a UUIDv7 but craftable by a malicious device —
// settle on the SAME record on every replica, and Merge resolves it identically
// to Apply. Without the lattice join this was last-write-by-order and diverged.
func TestStarredCollidingTagsConverge(t *testing.T) {
	t.Parallel()
	addX := StarChange{Op: OpStar, ItemID: "X", Tag: "same", At: 1}
	addY := StarChange{Op: OpStar, ItemID: "Y", Tag: "same", At: 2}
	xy := applyStarsIn([]StarChange{addX, addY})
	yx := applyStarsIn([]StarChange{addY, addX})
	if xy.Encode() != yx.Encode() {
		t.Fatalf("colliding tags diverged by order:\n%s\nvs\n%s", xy.Encode(), yx.Encode())
	}
	if MergeStars(applyStarsIn([]StarChange{addX}), applyStarsIn([]StarChange{addY})).Encode() != xy.Encode() {
		t.Fatal("MergeStars resolved a colliding tag differently from Apply")
	}
	// The deterministic winner is the lesser by (itemID, at): "X" < "Y".
	if ids := xy.StarredIDs(); len(ids) != 1 || ids[0] != "X" {
		t.Fatalf("colliding tags did not settle on the deterministic record: %v", ids)
	}
}

// TestStarredPoisonedClockDoesNotWrap: a star carrying At=MaxUint64 poisons the
// clock, but the next local Star must NOT wrap to 0 (which would sort new stars
// as the OLDEST). Saturating keeps them at MaxUint64.
func TestStarredPoisonedClockDoesNotWrap(t *testing.T) {
	t.Parallel()
	s := NewStarSet()
	s.Apply(StarChange{Op: OpStar, ItemID: "poison", Tag: "p", At: math.MaxUint64})
	c := s.Star("next")
	if c.At == 0 {
		t.Fatal("counter wrapped to 0 after MaxUint64")
	}
	if c.At != math.MaxUint64 {
		t.Fatalf("saturated At = %d, want MaxUint64", c.At)
	}
	if len(s.StarredIDs()) != 2 {
		t.Fatalf("a poisoned-clock star lost an item: %v", s.StarredIDs())
	}
}

// TestStarredZeroChangeIsAbsorbedDeterministically: a zero-value StarChange (a
// degenerate star) is absorbed order-independently.
func TestStarredZeroChangeIsAbsorbedDeterministically(t *testing.T) {
	t.Parallel()
	zero := StarChange{}
	real := StarChange{Op: OpStar, ItemID: "x", Tag: "t", At: 1}
	if applyStarsIn([]StarChange{zero, real}).Encode() != applyStarsIn([]StarChange{real, zero}).Encode() {
		t.Fatal("a zero change made the set order-dependent")
	}
}

// TestMergeStarsIgnoresNilStates: MergeStars tolerates nil inputs.
func TestMergeStarsIgnoresNilStates(t *testing.T) {
	t.Parallel()
	s := applyStarsIn([]StarChange{{Op: OpStar, ItemID: "x", Tag: "t", At: 1}})
	if MergeStars(nil, s, nil, nil).Encode() != s.Encode() {
		t.Fatal("nil states changed the merge result")
	}
}
