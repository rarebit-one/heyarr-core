package pieces

import (
	"strings"
	"testing"
)

// The third answer replication never had: "I have some of it, here is which".
func TestAvailabilityRecordsPartialHolding(t *testing.T) {
	a := NewAvailability(10)
	if a.Count() != 0 || a.Total() != 10 {
		t.Fatalf("a fresh availability is %d of %d, want 0 of 10", a.Count(), a.Total())
	}

	// Not 0, 1, 2 — a contiguous prefix makes "records these pieces" and
	// "records a count" indistinguishable, which is this repository's recurring
	// fixture flaw in another costume.
	for _, i := range []int{7, 2, 9} {
		a.Add(i)
	}
	if a.Count() != 3 {
		t.Errorf("count = %d after three adds, want 3", a.Count())
	}
	for _, i := range []int{2, 7, 9} {
		if !a.Has(i) {
			t.Errorf("piece %d was added and is not held", i)
		}
	}
	for _, i := range []int{0, 1, 3, 8} {
		if a.Has(i) {
			t.Errorf("piece %d is held and was never added", i)
		}
	}
}

// Adding a piece twice is safe: a handler that re-verifies a piece it already
// had must be re-runnable (invariant 9).
func TestAddingAPieceTwiceDoesNotDoubleCount(t *testing.T) {
	a := NewAvailability(4)
	a.Add(1)
	a.Add(1)
	a.Add(1)
	if a.Count() != 1 {
		t.Errorf("count = %d after adding the same piece three times, want 1", a.Count())
	}
}

// Out-of-range adds are ignored rather than panicking or growing the set.
func TestAnOutOfRangePieceIsNotRecorded(t *testing.T) {
	a := NewAvailability(4)
	a.Add(-1)
	a.Add(4)
	a.Add(1000)
	if a.Count() != 0 {
		t.Errorf("count = %d, want 0 — impossible pieces were recorded", a.Count())
	}
}

// Missing is what a session still needs, in a deterministic order so a test can
// assert which piece was asked for.
func TestMissingIsEverythingNotHeldInOrder(t *testing.T) {
	a := NewAvailability(5)
	a.Add(3)
	a.Add(0)

	got := a.Missing()
	want := []int{1, 2, 4}
	if len(got) != len(want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("missing = %v, want %v", got, want)
		}
	}
}

// Intersect is what this peer can usefully serve another — the question a swarm
// asks, and the one whose direction is easy to get backwards.
//
// The fixture is deliberately asymmetric: source and destination each hold
// something the other does not, so a reversed implementation returns a
// different answer rather than the same one.
func TestIntersectIsWhatTheSourceCanGiveTheDestination(t *testing.T) {
	source := NewAvailability(6)
	for _, i := range []int{0, 1, 4} {
		source.Add(i)
	}
	dest := NewAvailability(6)
	for _, i := range []int{1, 5} {
		dest.Add(i)
	}

	got := source.Intersect(dest)
	want := []int{0, 4} // held by the source, wanted by the destination
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("source.Intersect(dest) = %v, want %v — the direction is reversed", got, want)
	}

	// The reverse is a different set, which is what makes the assertion above
	// meaningful rather than symmetric.
	back := dest.Intersect(source)
	if len(back) != 1 || back[0] != 5 {
		t.Errorf("dest.Intersect(source) = %v, want [5]", back)
	}
}

// Availability survives the wire.
func TestAvailabilityRoundTripsThroughItsEncoding(t *testing.T) {
	a := NewAvailability(20)
	for _, i := range []int{0, 5, 13, 19} {
		a.Add(i)
	}

	back, err := DecodeAvailability(a.Encode(), 20)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Count() != a.Count() {
		t.Errorf("count = %d after a round trip, want %d", back.Count(), a.Count())
	}
	for i := range 20 {
		if back.Has(i) != a.Has(i) {
			t.Errorf("piece %d is %v after a round trip, want %v", i, back.Has(i), a.Has(i))
		}
	}
}

// A peer describing a different number of pieces is REFUSED, not reconciled.
//
// It means the two disagree about the geometry, and quietly accepting the
// overlap would mean requesting pieces whose boundaries are not shared — which
// produces bytes that fail verification for a reason nobody could diagnose.
func TestABitsetOfTheWrongWidthIsRefused(t *testing.T) {
	a := NewAvailability(20)
	a.Add(3)
	encoded := a.Encode()

	for _, n := range []int{8, 40, 21} {
		if _, err := DecodeAvailability(encoded, n); err == nil {
			t.Errorf("a %d-piece bitset was accepted as %d pieces", 20, n)
		} else if !strings.Contains(err.Error(), "geometry") {
			t.Errorf("the refusal does not say the peers disagree about geometry: %v", err)
		}
	}
}

// Bits set past the last piece do not make a peer look complete.
//
// The final byte of a bitset has spare bits whenever the count is not a
// multiple of eight, and a peer that sets them — by accident or otherwise —
// must not be believed to hold pieces that do not exist.
func TestBitsPastTheLastPieceDoNotInflateTheCount(t *testing.T) {
	// 12 pieces is two bytes with four spare bits in the second.
	full := "12:ffff"
	got, err := DecodeAvailability(full, 12)
	if err != nil {
		t.Fatal(err)
	}
	if got.Count() != 12 {
		t.Errorf("count = %d, want 12 — the four spare bits were counted", got.Count())
	}
	if got.Has(12) || got.Has(15) {
		t.Error("a piece past the end reports as held")
	}
}

// Complete is the whole point of the count, and it must not be true early.
func TestCompleteIsOnlyTrueWhenEveryPieceIsHeld(t *testing.T) {
	g, err := For(3 * MinPieceLength)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAvailability(g.Count())
	for i := range g.Count() {
		if g.Complete(a) {
			t.Fatalf("complete with %d of %d pieces", a.Count(), g.Count())
		}
		a.Add(i)
	}
	if !g.Complete(a) {
		t.Error("not complete with every piece held")
	}
}
