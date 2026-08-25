package cas

import (
	"bytes"
	"io"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// content is deterministic bytes of a given size, so a test can assert a
// digest. Not random: a fixture whose content changes between runs cannot be
// asserted against.
func digestOfBytes(t *testing.T, b []byte) hashing.Hash {
	t.Helper()
	h, _, err := hashing.HashReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func content(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = seed ^ byte(i%251)
	}
	return out
}

// Pieces arriving OUT OF ORDER assemble correctly and publish.
//
// This is §23's shape: a session pulling from several peers at once receives
// piece 2 before piece 0, and the append-only path could not express it at all.
func TestASparselyWrittenPartialAssemblesOutOfOrder(t *testing.T) {
	s := newStore(t)
	want := content(3000, 0x5a)
	digest := digestOfBytes(t, want)

	p, err := s.OpenPartial(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}

	// Deliberately last, first, middle — a sequential order would make "writes
	// at an offset" and "appends" indistinguishable.
	for _, at := range []int{2000, 0, 1000} {
		end := min(at+1000, len(want))
		if _, err := p.WriteAt(want[at:end], int64(at)); err != nil {
			t.Fatalf("WriteAt(%d): %v", at, err)
		}
	}

	desc, err := p.Publish(t.Context(), digest)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if desc.Hash != digest {
		t.Errorf("published %s, want %s", desc.Hash, digest)
	}

	rc, _, err := s.Open(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the assembled bytes are not what was written")
	}
}

// 🔴 A HOLE FAILS CLOSED. This is the whole safety argument of ADR-0043.
//
// A bitset that lies — by a bug, a torn write, or a crash between the write and
// the record — leaves a gap of zeroes. Publish re-reads and hashes the file
// WHOLE, so the digest does not match and nothing is published. The bitset is
// therefore free to be an optimisation and is never evidence.
//
// If this test ever fails, sparse writing is unsafe and ADR-0043 has to be
// re-made with the bitset promoted to evidence — a much stronger requirement.
func TestAPartialWithAHoleCannotBePublished(t *testing.T) {
	s := newStore(t)
	want := content(3000, 0x5a)
	digest := digestOfBytes(t, want)

	p, err := s.OpenPartial(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	// Everything except the middle third: exactly what a lost piece looks like.
	if _, err := p.WriteAt(want[0:1000], 0); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteAt(want[2000:3000], 2000); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Publish(t.Context(), digest); err == nil {
		t.Fatal("a partial with a hole was published — a file of zeroes was accepted " +
			"as received data, and sparse writing is not safe")
	}
	if has, _ := s.Has(t.Context(), digest); has {
		t.Error("the blob is in the store after a failed publish")
	}
}

// Size is the high-water mark for a sparse partial, not how much is present.
//
// Asserted because the doc says so and because code that reads it as progress
// will report a transfer nearly finished when its LAST piece happened to arrive
// first — which is exactly what this test constructs.
func TestSizeIsAHighWaterMarkNotProgress(t *testing.T) {
	s := newStore(t)
	want := content(3000, 0x11)
	digest := digestOfBytes(t, want)

	p, err := s.OpenPartial(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	// One piece, the last one.
	if _, err := p.WriteAt(want[2000:3000], 2000); err != nil {
		t.Fatal(err)
	}
	if got := p.Size(); got != 3000 {
		t.Fatalf("Size() = %d after writing the last third, want the 3000 high-water mark", got)
	}
	// The point: a third of the bytes are present and Size() says all of them.
	// Progress is the bitset's business, and this is why.
}

// A negative offset is refused rather than silently reinterpreted.
func TestANegativeOffsetIsRefused(t *testing.T) {
	s := newStore(t)
	digest := digestOfBytes(t, []byte("x"))
	p, err := s.OpenPartial(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	if _, err := p.WriteAt([]byte("no"), -1); err == nil {
		t.Error("a write at a negative offset was accepted")
	}
}

// Piece progress round-trips, and its absence is the ordinary first attempt
// rather than an error.
func TestPieceProgressRoundTripsAndAbsenceIsNotAnError(t *testing.T) {
	s := newStore(t)
	digest := digestOfBytes(t, []byte("some blob"))

	got, err := s.LoadPieceProgress(digest)
	if err != nil {
		t.Fatalf("loading progress that was never written: %v", err)
	}
	if got != "" {
		t.Errorf("a first attempt found progress %q", got)
	}

	const encoded = "12:ff0f"
	if err := s.SavePieceProgress(digest, encoded); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadPieceProgress(digest); err != nil || got != encoded {
		t.Errorf("progress round-tripped to %q (err %v), want %q", got, err, encoded)
	}

	if err := s.DiscardPieceProgress(digest); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LoadPieceProgress(digest); got != "" {
		t.Errorf("progress survived being discarded: %q", got)
	}
	// Discarding twice is cleanup, not an error.
	if err := s.DiscardPieceProgress(digest); err != nil {
		t.Errorf("discarding absent progress: %v", err)
	}
}

// Saving nothing removes the record, so "no record" and "a record of nothing"
// are one state rather than two that behave differently on resume.
func TestSavingEmptyProgressClearsIt(t *testing.T) {
	s := newStore(t)
	digest := digestOfBytes(t, []byte("another blob"))
	if err := s.SavePieceProgress(digest, "8:ff"); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePieceProgress(digest, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LoadPieceProgress(digest); got != "" {
		t.Errorf("progress is %q after being saved empty", got)
	}
}

// The append-only path is UNCHANGED. Adding a sparse write must not relax the
// sequential contract, whose warning about holes still applies to it.
func TestAppendStillOnlyGrowsFromTheEnd(t *testing.T) {
	s := newStore(t)
	want := content(2000, 0x77)
	digest := digestOfBytes(t, want)

	p, err := s.OpenPartial(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Append(t.Context(), bytes.NewReader(want[:1000])); err != nil {
		t.Fatal(err)
	}
	if got := p.Size(); got != 1000 {
		t.Fatalf("Size() = %d after appending 1000, want 1000", got)
	}
	if _, err := p.Append(t.Context(), bytes.NewReader(want[1000:])); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(t.Context(), digest); err != nil {
		t.Fatalf("the sequential path stopped working: %v", err)
	}
}

// A staging file can be read WHILE a transfer holds it.
//
// This is the swarm case: the transfer holding the partial is exactly the one
// whose bytes another peer wants, so an exclusive read would make a node unable
// to share precisely the blob it is working on (§23).
func TestAStagingFileCanBeReadWhileATransferHoldsIt(t *testing.T) {
	s := newStore(t)
	want := content(3000, 0x33)
	digest := digestOfBytes(t, want)

	p, err := s.OpenPartial(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if _, err := p.WriteAt(want[1000:2000], 1000); err != nil {
		t.Fatal(err)
	}

	// The partial is still held — OpenPartial would refuse a second caller.
	if _, err := s.OpenPartial(t.Context(), digest); err == nil {
		t.Fatal("a second OpenPartial succeeded, so this test is not exercising " +
			"the case it exists for")
	}

	got := make([]byte, 1000)
	n, err := s.ReadPartialAt(digest, got, 1000)
	if err != nil {
		t.Fatalf("reading a held staging file: %v", err)
	}
	if n != 1000 || !bytes.Equal(got, want[1000:2000]) {
		t.Error("the bytes read back are not the bytes written")
	}
}

// Reading a staging file that does not exist is ErrNotFound, not a panic and
// not an empty success — a peer asked for a blob it has no transfer of must be
// able to say so.
func TestReadingAnAbsentStagingFileSaysSo(t *testing.T) {
	s := newStore(t)
	digest := digestOfBytes(t, []byte("never staged"))
	if _, err := s.ReadPartialAt(digest, make([]byte, 8), 0); err == nil {
		t.Error("reading a staging file that was never created succeeded")
	}
}
