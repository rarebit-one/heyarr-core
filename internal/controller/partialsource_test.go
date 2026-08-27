package controller

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/pieces"
)

// These tests cover the piece→byte translation that keeps piece awareness OUT of
// the blob-serving package (webseed_test.go's guard): the adapter reads a real
// CAS availability record and answers in blob-absolute byte terms.

func stagePartial(t *testing.T, content []byte, landed ...int) (*cas.FS, hashing.Hash) {
	t.Helper()
	fs, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blob, _, err := hashing.HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	g, err := pieces.For(int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	p, err := fs.OpenPartial(context.Background(), blob)
	if err != nil {
		t.Fatal(err)
	}
	have := pieces.NewAvailability(g.Count())
	for _, i := range landed {
		off, length, rerr := g.Range(i)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if _, werr := p.WriteAt(content[off:off+length], off); werr != nil {
			t.Fatal(werr)
		}
		have.Add(i)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fs.SavePieceProgress(blob, pieces.Encode(g, have)); err != nil {
		t.Fatal(err)
	}
	return fs, blob
}

func threePieces() []byte {
	const size = 3 * pieces.MinPieceLength
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i%251 + 1)
	}
	return b
}

func newAdapter(fs *cas.FS) piecePartialSource {
	return piecePartialSource{store: fs, log: slog.New(slog.DiscardHandler)}
}

func TestPartialSourceArrivingSize(t *testing.T) {
	t.Parallel()
	content := threePieces()
	fs, blob := stagePartial(t, content, 0, 2)
	a := newAdapter(fs)

	size, inflight, err := a.ArrivingSize(context.Background(), blob)
	if err != nil {
		t.Fatal(err)
	}
	if !inflight {
		t.Fatal("a staged partial should report a transfer in flight")
	}
	if size != int64(len(content)) {
		t.Fatalf("size = %d, want the whole blob length %d", size, len(content))
	}
}

func TestPartialSourceAvailableTranslatesPiecesToBytes(t *testing.T) {
	t.Parallel()
	content := threePieces()
	fs, blob := stagePartial(t, content, 0, 2) // piece 1 is a hole
	a := newAdapter(fs)
	const pieceLen = pieces.MinPieceLength

	// Inside piece 0 (landed): readable up to the end of piece 0.
	until, ok, inflight, err := a.Available(context.Background(), blob, 1000)
	if err != nil || !ok || !inflight || until != pieceLen {
		t.Fatalf("piece 0: until=%d ok=%v inflight=%v err=%v; want until=%d ok=true inflight=true",
			until, ok, inflight, err, pieceLen)
	}
	// Inside piece 1 (the hole): not landed, still in flight.
	_, ok, inflight, err = a.Available(context.Background(), blob, pieceLen+50)
	if err != nil || ok || !inflight {
		t.Fatalf("piece 1 (hole): ok=%v inflight=%v err=%v; want ok=false inflight=true", ok, inflight, err)
	}
	// Inside piece 2 (landed): readable up to the end of the blob.
	until, ok, _, err = a.Available(context.Background(), blob, 2*pieceLen+50)
	if err != nil || !ok || until != int64(len(content)) {
		t.Fatalf("piece 2: until=%d ok=%v err=%v; want until=%d ok=true", until, ok, err, len(content))
	}
}

func TestPartialSourceReadPartialAtServesLandedBytes(t *testing.T) {
	t.Parallel()
	content := threePieces()
	fs, blob := stagePartial(t, content, 0, 2)
	a := newAdapter(fs)

	buf := make([]byte, 4096)
	n, err := a.ReadPartialAt(blob, buf, 1000)
	if err != nil || n != len(buf) {
		t.Fatalf("ReadPartialAt: n=%d err=%v", n, err)
	}
	if !bytes.Equal(buf, content[1000:1000+4096]) {
		t.Fatal("ReadPartialAt did not return the true content of a landed range")
	}
}

func TestPartialSourceAbsentAndZeroReportNotInflight(t *testing.T) {
	t.Parallel()
	fs, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newAdapter(fs)

	zero := hashing.MustParse("blake3:" + strings.Repeat("0", hashing.HexLen))
	absent, _, err := hashing.HashReader(bytes.NewReader([]byte("nothing has this")))
	if err != nil {
		t.Fatal(err)
	}
	for name, blob := range map[string]hashing.Hash{"zero": zero, "absent": absent} {
		size, inflight, aerr := a.ArrivingSize(context.Background(), blob)
		if aerr != nil || inflight || size != 0 {
			t.Fatalf("%s: size=%d inflight=%v err=%v; want 0, false, nil", name, size, inflight, aerr)
		}
	}
}

func TestPartialSourceReapedTransferReportsGone(t *testing.T) {
	t.Parallel()
	content := threePieces()
	fs, blob := stagePartial(t, content, 0)
	a := newAdapter(fs)

	if err := fs.DiscardPieceProgress(blob); err != nil {
		t.Fatal(err)
	}
	_, inflight, err := a.ArrivingSize(context.Background(), blob)
	if err != nil || inflight {
		t.Fatalf("after reap: inflight=%v err=%v; want not in flight", inflight, err)
	}
	if _, ok, inflight, err := a.Available(context.Background(), blob, 1000); err != nil || ok || inflight {
		t.Fatalf("after reap: ok=%v inflight=%v err=%v; want all false/nil", ok, inflight, err)
	}
}
