package transfer

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/pieces"
)

// fortyPieces is a geometry with enough pieces that a playback window in the
// middle is well clear of both ends.
func fortyPieces(t *testing.T) pieces.Geometry {
	t.Helper()
	g, err := pieces.For(40 * pieces.MinPieceLength)
	if err != nil {
		t.Fatal(err)
	}
	if g.Count() != 40 {
		t.Fatalf("geometry has %d pieces, want 40", g.Count())
	}
	return g
}

func claimHolding(g pieces.Geometry, indices ...int) *sourceClaim {
	have := pieces.NewAvailability(g.Count())
	for _, i := range indices {
		have.Add(i)
	}
	return &sourceClaim{claim: have}
}

func rangeIndices(from, to int) []int {
	out := make([]int, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// TestPlayheadPriorityBeatsRarity is the decision, and it is sabotage-verifiable:
// remove the window branch from assignLocked and this fails. Piece 0 is RAREST
// (held only by source A) and piece 20 is less rare (A and B). Rarest-first would
// pick 0; with the playhead at piece 20, the window fetches 20 first, because
// inside the window the player's order beats the swarm's.
func TestPlayheadPriorityBeatsRarity(t *testing.T) {
	t.Parallel()
	g := fortyPieces(t)
	s := newPieceSession(g, pieces.NewAvailability(g.Count()), nil)

	a := claimHolding(g, append([]int{0}, rangeIndices(20, 40)...)...) // holds 0 and 20..39
	b := claimHolding(g, rangeIndices(20, 40)...)                      // holds 20..39, NOT 0
	held := map[string]*sourceClaim{"A": a, "B": b}

	// Without a playhead: rarest-first picks the rarest piece A holds, which is 0.
	if idx, ok := s.assignLocked(a, held); !ok || idx != 0 {
		t.Fatalf("no playhead: assigned %d (ok=%v); rarest-first should pick the rarest piece, 0", idx, ok)
	}
	delete(s.inflight, 0) // release the reservation the assign made
	s.fetching--

	// With the playhead at piece 20: the window fetches 20 first, though it is
	// LESS rare than 0.
	s.playhead = 20
	if idx, ok := s.assignLocked(a, held); !ok || idx != 20 {
		t.Fatalf("playhead 20: assigned %d (ok=%v); the window should pick piece 20 ahead of the rarer piece 0", idx, ok)
	}
}

// TestPlayheadPriorityIsSequentialWithinWindow proves the window is served in
// order — the next piece the player needs, not the rarest in the window.
func TestPlayheadPriorityIsSequentialWithinWindow(t *testing.T) {
	t.Parallel()
	g := fortyPieces(t)
	s := newPieceSession(g, pieces.NewAvailability(g.Count()), nil)
	s.playhead = 20

	a := claimHolding(g, rangeIndices(0, 40)...)
	held := map[string]*sourceClaim{"A": a}

	// The window is [20,28); the lowest missing piece there is 20.
	idx, ok := s.assignLocked(a, held)
	if !ok || idx != 20 {
		t.Fatalf("assigned %d (ok=%v); want the window's lowest, 20", idx, ok)
	}
	// Mark 20 landed; the next window pick is 21.
	s.have.Add(20)
	delete(s.inflight, 20)
	s.fetching--
	if idx, ok := s.assignLocked(a, held); !ok || idx != 21 {
		t.Fatalf("assigned %d (ok=%v); after 20 lands the window should advance to 21", idx, ok)
	}
}

// TestPlayheadPriorityFallsBackOutsideWindow proves that when this source holds
// nothing in the window, the fetch reverts to rarest-first for the rest of the
// blob — the priority is an optimisation, not a restriction.
func TestPlayheadPriorityFallsBackOutsideWindow(t *testing.T) {
	t.Parallel()
	g := fortyPieces(t)
	s := newPieceSession(g, pieces.NewAvailability(g.Count()), nil)
	s.playhead = 20 // window [20,28)

	a := claimHolding(g, 3, 4, 5) // holds nothing in the window
	held := map[string]*sourceClaim{"A": a}

	idx, ok := s.assignLocked(a, held)
	if !ok || idx != 3 {
		t.Fatalf("assigned %d (ok=%v); with nothing in the window it should fall back to rarest-first (piece 3)", idx, ok)
	}
}

// TestRefreshPlayheadReadsAndConverts proves the driver reads the byte offset a
// consumer wrote and points the window at the piece that contains it — the
// role-legal crossing (invariant 4) and the offset→piece translation.
func TestRefreshPlayheadReadsAndConverts(t *testing.T) {
	t.Parallel()
	fs, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &Puller{store: fs, log: slog.New(slog.DiscardHandler)}
	blob, _, err := hashing.HashReader(bytes.NewReader([]byte("a blob being watched")))
	if err != nil {
		t.Fatal(err)
	}
	g := fortyPieces(t)

	// No playhead recorded: the window stays cleared.
	s := newPieceSession(g, pieces.NewAvailability(g.Count()), nil)
	p.refreshPlayhead(s, blob)
	if s.playhead != -1 {
		t.Fatalf("no playhead on disk: session playhead = %d, want -1", s.playhead)
	}

	// An offset in piece 20 points the window at 20.
	if err := fs.SavePlayhead(blob, int64(20)*g.PieceLength+100); err != nil {
		t.Fatal(err)
	}
	s2 := newPieceSession(g, pieces.NewAvailability(g.Count()), nil)
	p.refreshPlayhead(s2, blob)
	if s2.playhead != 20 {
		t.Fatalf("session playhead = %d, want 20 (the piece containing the recorded offset)", s2.playhead)
	}

	// An offset past the end of the blob is a stale hint and clears the window.
	if err := fs.SavePlayhead(blob, g.Size+1); err != nil {
		t.Fatal(err)
	}
	s3 := newPieceSession(g, pieces.NewAvailability(g.Count()), nil)
	p.refreshPlayhead(s3, blob)
	if s3.playhead != -1 {
		t.Fatalf("out-of-range playhead: session playhead = %d, want -1", s3.playhead)
	}
}
