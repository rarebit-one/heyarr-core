package pieces

import (
	"errors"
	"testing"
)

// Two peers must compute the same geometry from the same size, with nothing
// negotiated.
//
// This is the property the whole design rests on: a negotiated geometry is one
// a lying peer can move, and every piece hash a destination checks is only
// meaningful relative to the division that produced it.
func TestGeometryIsDerivedFromSizeAlone(t *testing.T) {
	for _, size := range []int64{1, 4096, 1 << 20, 100 << 20, 60 << 30} {
		first, err := For(size)
		if err != nil {
			t.Fatalf("For(%d): %v", size, err)
		}
		for range 5 {
			again, err := For(size)
			if err != nil {
				t.Fatal(err)
			}
			if again != first {
				t.Fatalf("For(%d) returned %+v then %+v — two peers would divide "+
					"the same blob differently", size, first, again)
			}
		}
	}
}

// The piece length is clamped at both ends, and the clamps are the point.
//
// A tiny file must not be split into meaningless fragments, and a very large
// one must not be split into a quarter of a million pieces whose bookkeeping
// costs more than the bytes.
func TestPieceLengthIsClampedAtBothEnds(t *testing.T) {
	tiny, err := For(1024)
	if err != nil {
		t.Fatal(err)
	}
	if tiny.PieceLength != MinPieceLength {
		t.Errorf("a 1 KiB blob has piece length %d, want the %d floor",
			tiny.PieceLength, MinPieceLength)
	}
	if tiny.Count() != 1 {
		t.Errorf("a 1 KiB blob is %d pieces, want 1", tiny.Count())
	}

	huge, err := For(1 << 40) // 1 TiB
	if err != nil {
		t.Fatal(err)
	}
	if huge.PieceLength != MaxPieceLength {
		t.Errorf("a 1 TiB blob has piece length %d, want the %d ceiling",
			huge.PieceLength, MaxPieceLength)
	}
}

// Between the clamps the length scales, so the piece COUNT stays bounded
// instead of the piece SIZE staying fixed.
//
// Asserted as a bound on the count rather than as an exact length: the count is
// what the design cares about, and an exact length would pin the formula rather
// than the property. That is this session's recurring lesson — assert the
// fixture-independent thing.
func TestThePieceCountStaysBoundedAsBlobsGrow(t *testing.T) {
	for _, size := range []int64{
		1 << 20, 16 << 20, 256 << 20, 1 << 30, 8 << 30, 64 << 30,
	} {
		g, err := For(size)
		if err != nil {
			t.Fatal(err)
		}
		n := g.Count()
		// Two regimes, and the bound is whichever applies.
		//
		// Below the ceiling the length scales so the count stays near the
		// target. Once the length is clamped at MaxPieceLength the count has to
		// grow, and the floor on it is size/MaxPieceLength — so the bound is
		// the larger of the two, not the target alone.
		atCeiling := int((size + MaxPieceLength - 1) / MaxPieceLength)
		bound := targetPieceCount + 1
		if atCeiling > bound {
			bound = atCeiling
		}
		if n > bound {
			t.Errorf("size %d is %d pieces, above the %d this geometry allows", size, n, bound)
		}
		// And the claim that actually matters: far fewer than a fixed floor
		// would give. A 64 GiB blob at 256 KiB is a quarter of a million
		// pieces, which is the bookkeeping this scaling exists to avoid.
		if fixed := int(size / MinPieceLength); fixed > targetPieceCount && n >= fixed {
			t.Errorf("size %d is %d pieces, no better than the %d a fixed floor would give",
				size, n, fixed)
		}
		if n < 1 {
			t.Errorf("size %d is %d pieces", size, n)
		}
		t.Logf("size=%-12d piece_length=%-9d count=%d", size, g.PieceLength, n)
	}
}

// The last piece is short, is a piece like any other, and the ranges tile the
// blob exactly.
//
// A size deliberately NOT a multiple of the piece length, because a size that
// divides evenly makes "handles the short last piece" and "does not handle it"
// indistinguishable — the same shape as a fixture whose subject sits at
// position zero.
func TestRangesTileTheBlobExactlyIncludingAShortLastPiece(t *testing.T) {
	const size = (3 * MinPieceLength) + 7
	g, err := For(size)
	if err != nil {
		t.Fatal(err)
	}
	if g.Count() != 4 {
		t.Fatalf("count = %d, want 4 (three full pieces and a short one)", g.Count())
	}

	var covered int64
	var prevEnd int64
	for i := range g.Count() {
		off, length, err := g.Range(i)
		if err != nil {
			t.Fatalf("Range(%d): %v", i, err)
		}
		if off != prevEnd {
			t.Errorf("piece %d starts at %d, previous ended at %d — the pieces do not tile",
				i, off, prevEnd)
		}
		if length <= 0 {
			t.Errorf("piece %d has length %d", i, length)
		}
		prevEnd = off + length
		covered += length
	}
	if covered != size {
		t.Errorf("the pieces cover %d bytes of a %d byte blob", covered, size)
	}
	if _, last, _ := g.Range(g.Count() - 1); last != 7 {
		t.Errorf("the last piece is %d bytes, want the 7 that are left over", last)
	}
}

// A piece index that does not exist is refused rather than clamped.
func TestAnImpossiblePieceIsRefused(t *testing.T) {
	g, err := For(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range []int{-1, g.Count(), g.Count() + 100} {
		if _, _, err := g.Range(idx); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("Range(%d) returned %v, want ErrOutOfRange", idx, err)
		}
	}
}

// A zero-length blob has no pieces and says so, rather than returning a
// geometry with a count of zero that a caller would loop over happily.
func TestAZeroLengthBlobIsRefused(t *testing.T) {
	if _, err := For(0); !errors.Is(err, ErrEmptyBlob) {
		t.Errorf("For(0) returned %v, want ErrEmptyBlob", err)
	}
}

// A verified prefix becomes a piece index, which is how M5's resumption
// (ADR-0035) turns into availability.
func TestAVerifiedPrefixMapsToAPieceIndex(t *testing.T) {
	g, err := For(10 * MinPieceLength)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		offset int64
		want   int
	}{
		{0, 0},
		{MinPieceLength - 1, 0},
		{MinPieceLength, 1},
		{3*MinPieceLength + 5, 3},
	} {
		got, err := g.IndexAt(tc.offset)
		if err != nil {
			t.Fatalf("IndexAt(%d): %v", tc.offset, err)
		}
		if got != tc.want {
			t.Errorf("IndexAt(%d) = %d, want %d", tc.offset, got, tc.want)
		}
	}
	if _, err := g.IndexAt(g.Size); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("IndexAt at the end returned %v, want ErrOutOfRange", err)
	}
}
