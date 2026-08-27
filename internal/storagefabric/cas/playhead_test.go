package cas

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

func playheadBlob(t *testing.T) hashing.Hash {
	t.Helper()
	h, _, err := hashing.HashReader(bytes.NewReader([]byte("a blob a consumer is reading")))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPlayheadRoundTrip(t *testing.T) {
	t.Parallel()
	fs, err := OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blob := playheadBlob(t)

	// Absent before anything is written.
	if _, ok, err := fs.LoadPlayhead(blob); err != nil || ok {
		t.Fatalf("absent playhead: ok=%v err=%v; want ok=false", ok, err)
	}

	if err := fs.SavePlayhead(blob, 5242880); err != nil {
		t.Fatal(err)
	}
	off, ok, err := fs.LoadPlayhead(blob)
	if err != nil || !ok || off != 5242880 {
		t.Fatalf("loaded off=%d ok=%v err=%v; want 5242880, true", off, ok, err)
	}

	// Rewritten in place, as the read position moves.
	if err := fs.SavePlayhead(blob, 7340032); err != nil {
		t.Fatal(err)
	}
	if off, _, _ := fs.LoadPlayhead(blob); off != 7340032 {
		t.Fatalf("after rewrite off=%d; want 7340032", off)
	}

	if err := fs.DiscardPlayhead(blob); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := fs.LoadPlayhead(blob); ok {
		t.Fatal("playhead still present after discard")
	}
}

func TestPlayheadRefusesBadInputs(t *testing.T) {
	t.Parallel()
	fs, err := OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.SavePlayhead(hashing.Hash{}, 10); err == nil {
		t.Fatal("SavePlayhead accepted a zero digest")
	}
	if err := fs.SavePlayhead(playheadBlob(t), -1); err == nil {
		t.Fatal("SavePlayhead accepted a negative offset")
	}
	if _, _, err := fs.LoadPlayhead(hashing.Hash{}); err == nil {
		t.Fatal("LoadPlayhead accepted a zero digest")
	}
}

// TestPlayheadIsReapedWithThePartial proves the record does not outlive the
// transfer it steers: reaping the abandoned staging file takes the playhead (and
// the availability record) with it, even though neither is a .part file
// TempFiles reports on its own.
func TestPlayheadIsReapedWithThePartial(t *testing.T) {
	t.Parallel()
	blob := playheadBlob(t)
	if !strings.HasPrefix(PlayheadName(blob), partialPrefix) {
		t.Fatalf("playhead name %q does not share the partial prefix %q", PlayheadName(blob), partialPrefix)
	}

	fs, err := OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// An interrupted transfer leaves a staging file and its sidecars.
	p, err := fs.OpenPartial(context.Background(), blob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteAt([]byte("some bytes"), 0); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fs.SavePieceProgress(blob, "0:1:x"); err != nil {
		t.Fatal(err)
	}
	if err := fs.SavePlayhead(blob, 42); err != nil {
		t.Fatal(err)
	}

	// Reap everything old (a future cutoff makes every file old).
	if _, err := fs.ReapTemp(-time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := fs.LoadPlayhead(blob); ok {
		t.Fatal("the playhead survived the reap of its partial")
	}
	if enc, _ := fs.LoadPieceProgress(blob); enc != "" {
		t.Fatal("the availability record survived the reap of its partial")
	}
}
