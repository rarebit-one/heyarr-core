package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/pieces"
)

// --content-in stages pieces of REAL, pre-existing bytes, keeping THEIR digest —
// which is what lets one swarm node be pre-staged as a deterministic piece source
// for another instead of racing a live transfer (#274). The digest must be the
// bytes' own, and exactly the named pieces must land.
func TestContentInStagesRealPiecesUnderTheBytesOwnDigest(t *testing.T) {
	// A size with several pieces, so "some landed, some not" is a real state.
	const size = 786432
	content := make([]byte, size)
	for i := range content {
		// Deterministic, and never zero, so a landed piece is distinguishable
		// from a hole (which reads back as zeroes).
		content[i] = byte(i%251) + 1
	}
	dir := t.TempDir()
	contentFile := filepath.Join(dir, "real.bin")
	if err := os.WriteFile(contentFile, content, 0o600); err != nil {
		t.Fatal(err)
	}
	casRoot := filepath.Join(dir, "cas")

	// Stage pieces 0 and 2 of the real bytes; leave 1 a hole.
	if err := run(casRoot, 0, "0,2", 0, "", contentFile); err != nil {
		t.Fatalf("run with --content-in: %v", err)
	}

	// The digest the partial is addressed by must be the CONTENT's, not a
	// synthetic seed's — that is the whole point of --content-in.
	want, _, err := hashing.HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}

	fs, err := cas.OpenFS(casRoot)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := fs.LoadPieceProgress(want)
	if err != nil {
		t.Fatalf("no progress recorded under the content's digest: %v", err)
	}
	if encoded == "" {
		t.Fatal("the partial recorded no progress under the content's own digest")
	}
	g, have, err := pieces.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}

	if !have.Has(0) || !have.Has(2) {
		t.Errorf("pieces 0 and 2 were staged but the record does not hold them")
	}
	if have.Has(1) {
		t.Errorf("piece 1 was a hole but the record claims it landed")
	}
	if have.Count() != 2 {
		t.Errorf("staged 2 pieces, record holds %d", have.Count())
	}

	// And the landed bytes are the REAL bytes: read piece 0 back and compare.
	p, err := fs.OpenPartial(t.Context(), want)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	off, length, err := g.Range(0)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, length)
	if _, err := p.ReadAt(got, off); err != nil {
		t.Fatalf("reading staged piece 0: %v", err)
	}
	if !bytes.Equal(got, content[off:off+length]) {
		t.Error("staged piece 0 is not the real content's bytes")
	}
}

// --content-in with a --size that disagrees with the file is refused, so a
// caller cannot stage pieces of one blob believing it is another.
func TestContentInRejectsASizeMismatch(t *testing.T) {
	dir := t.TempDir()
	contentFile := filepath.Join(dir, "real.bin")
	if err := os.WriteFile(contentFile, make([]byte, 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(filepath.Join(dir, "cas"), 999, "0", 0, "", contentFile)
	if err == nil {
		t.Fatal("a size that disagrees with the content file was accepted")
	}
}
