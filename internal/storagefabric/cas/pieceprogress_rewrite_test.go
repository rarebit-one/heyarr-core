package cas_test

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// 🔴 Piece progress is written ONCE PER PIECE, so the second save must work.
//
// It did not. The record was written with the blob permission — read-only,
// which is right for an immutable blob and wrong for a file that is replaced on
// every piece — so the first save succeeded and every later one failed with
// EACCES. Losing a hint is not fatal and the failure is logged at debug, so
// nothing was red: the symptom was a peer that advertised its first piece and
// never any of the others, which is §23 quietly not happening.
func TestPieceProgressCanBeRewritten(t *testing.T) {
	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hashing.New()
	if _, err := h.Write([]byte("a blob whose pieces arrive one at a time")); err != nil {
		t.Fatal(err)
	}
	blob := h.Sum()

	for i, want := range []string{"first", "second", "third"} {
		if err := store.SavePieceProgress(blob, want); err != nil {
			t.Fatalf("save %d of the piece progress failed: %v", i+1, err)
		}
		got, err := store.LoadPieceProgress(blob)
		if err != nil {
			t.Fatalf("reading back save %d: %v", i+1, err)
		}
		if got != want {
			t.Errorf("save %d read back as %q, want %q", i+1, got, want)
		}
	}
}
