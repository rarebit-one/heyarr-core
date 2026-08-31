package crdt

import (
	"math"
	"math/rand"
	"testing"
)

// Adversarial synthetic tests for the reading-position LWW register: hostile
// PositionChange values a malicious device could craft (§42) — colliding order
// keys, a Lamport counter at the ceiling, a zero write. Every replica must still
// converge to the same map regardless of arrival order (§43), degrade
// predictably, or have its boundary pinned here.

func applyPositionsIn(changes []PositionChange) *ReadingPositions {
	r := NewReadingPositions()
	r.Apply(changes...)
	return r
}

func shuffledPositions(changes []PositionChange, seed int64) []PositionChange {
	out := append([]PositionChange(nil), changes...)
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test shuffle
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestReadingConvergesUnderHostileOrdering: a pile of writes across publications,
// some colliding on (At, Writer), applied in 200 random orders, all converge to
// one byte-identical map.
func TestReadingConvergesUnderHostileOrdering(t *testing.T) {
	t.Parallel()
	var changes []PositionChange
	for i := 0; i < 20; i++ {
		changes = append(changes, PositionChange{
			PubID:    "book-" + string(rune('a'+(i%4))),
			Position: "pos-" + string(rune('a'+i)),
			At:       uint64(i % 5),
			Writer:   PosTag("w-" + string(rune('a'+(i%6)))),
		})
	}
	// Two writes fully colliding on (At, Writer) but different positions.
	changes = append(changes,
		PositionChange{PubID: "book-a", Position: "zzz", At: 9, Writer: "same"},
		PositionChange{PubID: "book-a", Position: "aaa", At: 9, Writer: "same"},
	)

	want := applyPositionsIn(changes).Encode()
	for seed := int64(0); seed < 200; seed++ {
		if got := applyPositionsIn(shuffledPositions(changes, seed)).Encode(); got != want {
			t.Fatalf("permutation seed=%d diverged:\n got=%s\nwant=%s", seed, got, want)
		}
	}
}

// TestReadingFullyCollidingKeyBreaksOnPosition: two writes with the SAME (At,
// Writer) but different positions — which a real UUIDv7 Writer never produces —
// settle on the same position on every replica (the greater string), and Merge
// resolves it identically to Apply. Without the value tie-break this was
// last-write-by-order and two replicas DIVERGED.
func TestReadingFullyCollidingKeyBreaksOnPosition(t *testing.T) {
	t.Parallel()
	lo := PositionChange{PubID: "b", Position: "aaa", At: 3, Writer: "dup"}
	hi := PositionChange{PubID: "b", Position: "zzz", At: 3, Writer: "dup"}

	loHi := applyPositionsIn([]PositionChange{lo, hi})
	hiLo := applyPositionsIn([]PositionChange{hi, lo})
	if loHi.Encode() != hiLo.Encode() {
		t.Fatalf("fully-colliding key diverged by order:\n%s\nvs\n%s", loHi.Encode(), hiLo.Encode())
	}
	if MergeReadingPositions(applyPositionsIn([]PositionChange{lo}), applyPositionsIn([]PositionChange{hi})).Encode() != loHi.Encode() {
		t.Fatal("Merge resolved a fully-colliding key differently from Apply")
	}
	// The greater position string ("zzz") wins on every replica.
	if got, _ := loHi.Position("b"); got != "zzz" {
		t.Fatalf("the value tie-break did not settle deterministically: got %q", got)
	}
}

// TestReadingReplayIsIdempotent: replaying writes any number of times equals
// applying them once.
func TestReadingReplayIsIdempotent(t *testing.T) {
	t.Parallel()
	changes := []PositionChange{
		{PubID: "a", Position: "p1", At: 1, Writer: "w1"},
		{PubID: "a", Position: "p2", At: 2, Writer: "w2"},
		{PubID: "b", Position: "q1", At: 1, Writer: "w3"},
	}
	once := applyPositionsIn(changes).Encode()
	many := NewReadingPositions()
	for i := 0; i < 50; i++ {
		many.Apply(shuffledPositions(changes, int64(i))...)
	}
	if many.Encode() != once {
		t.Fatal("replaying writes changed the map: not idempotent")
	}
}

// TestReadingPoisonedClockDoesNotWrap: a write carrying At=MaxUint64 poisons the
// clock; the next local Set must NOT wrap to 0. Saturating keeps it at
// MaxUint64. This pins an honest LWW limitation: a MaxUint64 write outranks every
// later local write by At and can only be displaced by a greater Writer tag —
// convergence still holds, but the poison sticks, deterministically.
func TestReadingPoisonedClockDoesNotWrap(t *testing.T) {
	t.Parallel()
	r := NewReadingPositions()
	r.Apply(PositionChange{PubID: "x", Position: "poison", At: math.MaxUint64, Writer: "zzzz-poison"})
	c := r.Set("x", "next")
	if c.At == 0 {
		t.Fatal("counter wrapped to 0 after MaxUint64")
	}
	if c.At != math.MaxUint64 {
		t.Fatalf("saturated At = %d, want MaxUint64", c.At)
	}
	// Convergence is what matters: whoever wins, both apply orders agree.
	if applyPositionsIn([]PositionChange{{PubID: "x", Position: "poison", At: math.MaxUint64, Writer: "zzzz-poison"}, c}).Encode() !=
		applyPositionsIn([]PositionChange{c, {PubID: "x", Position: "poison", At: math.MaxUint64, Writer: "zzzz-poison"}}).Encode() {
		t.Fatal("a poisoned-clock write made the map order-dependent")
	}
}

// TestReadingZeroChangeIsAbsorbedDeterministically: a zero-value PositionChange
// (empty pub, empty position, At 0) is absorbed order-independently.
func TestReadingZeroChangeIsAbsorbedDeterministically(t *testing.T) {
	t.Parallel()
	zero := PositionChange{}
	real := PositionChange{PubID: "x", Position: "p", At: 1, Writer: "w"}
	if applyPositionsIn([]PositionChange{zero, real}).Encode() != applyPositionsIn([]PositionChange{real, zero}).Encode() {
		t.Fatal("a zero change made the map order-dependent")
	}
}

// TestMergeReadingPositionsIgnoresNilStates: MergeReadingPositions tolerates nil.
func TestMergeReadingPositionsIgnoresNilStates(t *testing.T) {
	t.Parallel()
	r := applyPositionsIn([]PositionChange{{PubID: "x", Position: "p", At: 1, Writer: "w"}})
	if MergeReadingPositions(nil, r, nil, nil).Encode() != r.Encode() {
		t.Fatal("nil states changed the merge result")
	}
}
