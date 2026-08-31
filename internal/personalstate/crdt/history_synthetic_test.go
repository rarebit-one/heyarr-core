package crdt

import (
	"math"
	"math/rand"
	"testing"
)

// Adversarial synthetic tests for the play-history G-Set: hostile PlayChange
// values a malicious device could craft (§42) — a colliding event tag, a Lamport
// counter at the ceiling, a zero event, a replayed event. Every replica must
// still converge to the same log regardless of arrival order (§43), degrade
// predictably, or have its boundary pinned here.

func applyPlaysIn(changes []PlayChange) *PlayLog {
	l := NewPlayLog()
	l.Apply(changes...)
	return l
}

func shuffledPlays(changes []PlayChange, seed int64) []PlayChange {
	out := append([]PlayChange(nil), changes...)
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test shuffle
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestPlayConvergesUnderHostileOrdering: a pile of events across items, some
// colliding on tag, applied in 200 random orders, all converge to one
// byte-identical log.
func TestPlayConvergesUnderHostileOrdering(t *testing.T) {
	t.Parallel()
	var changes []PlayChange
	for i := 0; i < 20; i++ {
		changes = append(changes, PlayChange{
			ItemID: "item-" + string(rune('a'+(i%5))),
			Tag:    PlayTag(string(rune('a'+i)) + "-tag"),
			At:     uint64(i % 7),
		})
	}
	want := applyPlaysIn(changes).Encode()
	for seed := int64(0); seed < 200; seed++ {
		if got := applyPlaysIn(shuffledPlays(changes, seed)).Encode(); got != want {
			t.Fatalf("permutation seed=%d diverged:\n got=%s\nwant=%s", seed, got, want)
		}
	}
}

// TestPlayReplayIsIdempotent: replaying events any number of times equals
// applying them once — a lossy or malicious relay resending cannot inflate a
// play count.
func TestPlayReplayIsIdempotent(t *testing.T) {
	t.Parallel()
	changes := []PlayChange{
		{ItemID: "a", Tag: "ta", At: 1},
		{ItemID: "a", Tag: "tb", At: 2},
		{ItemID: "b", Tag: "tc", At: 3},
	}
	once := applyPlaysIn(changes).Encode()
	many := NewPlayLog()
	for i := 0; i < 50; i++ {
		many.Apply(shuffledPlays(changes, int64(i))...)
	}
	if many.Encode() != once {
		t.Fatal("replaying events changed the log: not idempotent")
	}
	// And the count did not inflate: two distinct tags for "a".
	if got := applyPlaysIn(changes).Count("a"); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
}

// TestPlayCollidingTagsConverge: two events under the SAME tag with different
// payloads — impossible from a UUIDv7 but craftable — settle on the SAME record
// on every replica, and Merge resolves it identically to Apply.
func TestPlayCollidingTagsConverge(t *testing.T) {
	t.Parallel()
	addX := PlayChange{ItemID: "X", Tag: "same", At: 1}
	addY := PlayChange{ItemID: "Y", Tag: "same", At: 2}
	xy := applyPlaysIn([]PlayChange{addX, addY})
	yx := applyPlaysIn([]PlayChange{addY, addX})
	if xy.Encode() != yx.Encode() {
		t.Fatalf("colliding tags diverged by order:\n%s\nvs\n%s", xy.Encode(), yx.Encode())
	}
	if MergePlayLogs(applyPlaysIn([]PlayChange{addX}), applyPlaysIn([]PlayChange{addY})).Encode() != xy.Encode() {
		t.Fatal("MergePlayLogs resolved a colliding tag differently from Apply")
	}
	// A colliding tag collapses to ONE event (the lesser record: item "X").
	if got := xy.Count("X"); got != 1 {
		t.Fatalf("colliding tag did not collapse to one event: Count(X)=%d", got)
	}
	if got := xy.Count("Y"); got != 0 {
		t.Fatalf("the losing colliding record survived: Count(Y)=%d", got)
	}
}

// TestPlayPoisonedClockDoesNotWrap: an event carrying At=MaxUint64 poisons the
// clock, but the next local Record must NOT wrap to 0 (which would make new plays
// the oldest). Saturating keeps them at MaxUint64.
func TestPlayPoisonedClockDoesNotWrap(t *testing.T) {
	t.Parallel()
	l := NewPlayLog()
	l.Apply(PlayChange{ItemID: "poison", Tag: "p", At: math.MaxUint64})
	c := l.Record("next")
	if c.At == 0 {
		t.Fatal("counter wrapped to 0 after MaxUint64")
	}
	if c.At != math.MaxUint64 {
		t.Fatalf("saturated At = %d, want MaxUint64", c.At)
	}
	if l.Count("next") != 1 || l.Count("poison") != 1 {
		t.Fatalf("a poisoned-clock event lost a play: %s", l.Encode())
	}
}

// TestPlayZeroChangeIsAbsorbedDeterministically: a zero-value PlayChange (empty
// item, empty tag, At 0) is absorbed order-independently.
func TestPlayZeroChangeIsAbsorbedDeterministically(t *testing.T) {
	t.Parallel()
	zero := PlayChange{}
	real := PlayChange{ItemID: "x", Tag: "t", At: 1}
	if applyPlaysIn([]PlayChange{zero, real}).Encode() != applyPlaysIn([]PlayChange{real, zero}).Encode() {
		t.Fatal("a zero change made the log order-dependent")
	}
}

// TestMergePlayLogsIgnoresNilStates: MergePlayLogs tolerates nil inputs.
func TestMergePlayLogsIgnoresNilStates(t *testing.T) {
	t.Parallel()
	l := applyPlaysIn([]PlayChange{{ItemID: "x", Tag: "t", At: 1}})
	if MergePlayLogs(nil, l, nil, nil).Encode() != l.Encode() {
		t.Fatal("nil states changed the merge result")
	}
}
