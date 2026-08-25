package pieces

import (
	"fmt"
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

// Availability and its geometry survive the wire together.
func TestAvailabilityRoundTripsThroughItsEncoding(t *testing.T) {
	g, err := For(20 * MinPieceLength)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAvailability(g.Count())
	for _, i := range []int{0, 5, 13, 19} {
		a.Add(i)
	}

	backG, back, err := Decode(Encode(g, a))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if backG != g {
		t.Errorf("geometry = %+v after a round trip, want %+v", backG, g)
	}
	if back.Count() != a.Count() {
		t.Errorf("count = %d after a round trip, want %d", back.Count(), a.Count())
	}
	for i := range g.Count() {
		if back.Has(i) != a.Has(i) {
			t.Errorf("piece %d is %v after a round trip, want %v", i, back.Has(i), a.Has(i))
		}
	}
}

// 🔴 The SIZE is what the geometry is derived from, and the count alone could
// not have done it.
//
// This is why the encoding carries a size rather than a piece count. 1024
// pieces is a 256 MiB blob at 256 KiB pieces AND an 8 GiB blob at 8 MiB pieces,
// so a node told only "1024 pieces" cannot work out where piece 5 starts —
// which is exactly what a node serving from a partial has to do, because a
// partial's length is a high-water mark and not the blob's size (ADR-0043).
func TestTwoBlobsWithTheSamePieceCountHaveDifferentGeometries(t *testing.T) {
	small, err := For(256 << 20)
	if err != nil {
		t.Fatal(err)
	}
	large, err := For(8 << 30)
	if err != nil {
		t.Fatal(err)
	}
	if small.Count() != large.Count() {
		t.Skipf("the fixture no longer produces equal counts (%d and %d); pick two sizes that do",
			small.Count(), large.Count())
	}
	if small.PieceLength == large.PieceLength {
		t.Fatal("two blobs with the same piece count also have the same piece length, " +
			"so this test asserts nothing")
	}

	// Both encode the same bitset width; only the size tells them apart.
	a := NewAvailability(small.Count())
	a.Add(5)
	gotSmall, _, err := Decode(Encode(small, a))
	if err != nil {
		t.Fatal(err)
	}
	gotLarge, _, err := Decode(Encode(large, a))
	if err != nil {
		t.Fatal(err)
	}
	if gotSmall.PieceLength == gotLarge.PieceLength {
		t.Error("both decoded to the same piece length, so a serving node would read " +
			"the wrong bytes for one of them")
	}
	offSmall, _, _ := gotSmall.Range(5)
	offLarge, _, _ := gotLarge.Range(5)
	if offSmall == offLarge {
		t.Error("piece 5 starts at the same offset in both, which cannot be right")
	}
}

// A peer describing a different number of pieces is REFUSED, not reconciled.
//
// It means the two disagree about the geometry, and quietly accepting the
// overlap would mean requesting pieces whose boundaries are not shared — which
// produces bytes that fail verification for a reason nobody could diagnose.
func TestABitsetOfTheWrongWidthIsRefused(t *testing.T) {
	g, err := For(20 * MinPieceLength)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAvailability(g.Count())
	a.Add(3)
	encoded := Encode(g, a)

	// Keep the bitset, claim a different blob size. The width check is now
	// EXACT rather than approximate, because the count is derived from the
	// size rather than claimed beside it.
	_, hex, _ := strings.Cut(encoded, ":")
	for _, size := range []int64{MinPieceLength, 400 * MinPieceLength} {
		if _, _, err := Decode(fmt.Sprintf("%d:%s", size, hex)); err == nil {
			t.Errorf("a 20-piece bitset was accepted for a %d byte blob", size)
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
	// A size whose piece count is not a multiple of eight, so the last byte of
	// the bitset has spare bits.
	g, err := For(12 * MinPieceLength)
	if err != nil {
		t.Fatal(err)
	}
	if g.Count()%8 == 0 {
		t.Skipf("the fixture's count (%d) is a multiple of 8, so there are no spare bits",
			g.Count())
	}
	width := (g.Count() + 7) / 8
	full := fmt.Sprintf("%d:%s", g.Size, strings.Repeat("ff", width))

	_, got, err := Decode(full)
	if err != nil {
		t.Fatal(err)
	}
	if got.Count() != g.Count() {
		t.Errorf("count = %d, want %d — the spare bits were counted", got.Count(), g.Count())
	}
	if got.Has(g.Count()) {
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
