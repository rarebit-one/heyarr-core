package chunking

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// pseudoRandom generates deterministic bytes from an xorshift64* generator
// written out here in full.
//
// It is not math/rand and not crypto/rand deliberately. This package's whole
// claim is that the same bytes cut identically on every platform, so the test
// inputs themselves must be reproducible from committed source on every
// platform — a fixture that depends on a standard-library generator's internal
// algorithm would move the pin somewhere we do not control. The generator is
// also why the golden fixture needs no testdata file: reading one would mean
// importing os, which is exactly what depguard forbids here.
func pseudoRandom(n int, seed uint64) []byte {
	state := seed
	if state == 0 {
		state = 1
	}
	out := make([]byte, 0, n+8)
	var word [8]byte
	for len(out) < n {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		binary.LittleEndian.PutUint64(word[:], state*0x2545F4914F6CDD1D)
		out = append(out, word[:]...)
	}
	return out[:n]
}

// framedReader delivers its content in reads of at most frame bytes. The
// chunker's output must not depend on it (see TestChunksAreIndependentOfReadFraming).
type framedReader struct {
	data  []byte
	frame int
	pos   int
}

func (f *framedReader) Read(p []byte) (int, error) {
	if f.pos >= len(f.data) {
		return 0, io.EOF
	}
	n := min(len(p), f.frame, len(f.data)-f.pos)
	copy(p, f.data[f.pos:f.pos+n])
	f.pos += n
	return n, nil
}

// collect drains a chunker into a slice. Production callers stream; tests need
// the whole sequence to assert on it.
func collect(t *testing.T, r io.Reader, cfg Config) []Chunk {
	t.Helper()
	c, err := New(r, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var chunks []Chunk
	for {
		chunk, err := c.Next()
		if errors.Is(err, io.EOF) {
			return chunks
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		chunks = append(chunks, chunk)
	}
}

func chunkBytes(t *testing.T, data []byte, cfg Config) []Chunk {
	t.Helper()
	return collect(t, bytes.NewReader(data), cfg)
}

func digestOf(t *testing.T, b []byte) hashing.Hash {
	t.Helper()
	h, _, err := hashing.HashReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// digestSet is the reuse question in one value: which chunk contents does this
// sequence contain?
func digestSet(chunks []Chunk) map[hashing.Hash]int {
	set := make(map[hashing.Hash]int, len(chunks))
	for _, c := range chunks {
		set[c.Digest]++
	}
	return set
}

// survivingFraction is the fraction of a's chunks whose exact bytes still
// appear as a chunk in b. This is the number chunk reuse across a modified
// file is worth.
func survivingFraction(a, b []Chunk) float64 {
	if len(a) == 0 {
		return 0
	}
	have := digestSet(b)
	survived := 0
	for _, c := range a {
		if have[c.Digest] > 0 {
			have[c.Digest]--
			survived++
		}
	}
	return float64(survived) / float64(len(a))
}

// fixedSizeChunks is the control for the shift property: the chunker FastCDC
// exists to not be. Cut points come from position alone, so an insertion at the
// front moves every one of them.
func fixedSizeChunks(t *testing.T, data []byte, size int) []Chunk {
	t.Helper()
	var chunks []Chunk
	for off := 0; off < len(data); off += size {
		end := min(off+size, len(data))
		chunks = append(chunks, Chunk{
			Offset: int64(off),
			Length: int64(end - off),
			Digest: digestOf(t, data[off:end]),
		})
	}
	return chunks
}

// generatedReader produces deterministic bytes on demand without ever holding
// them. The flat-memory test needs an input far larger than anything that
// should sit in the heap, so materialising it would measure the test's
// appetite rather than the chunker's.
type generatedReader struct {
	remaining int64
	state     uint64
}

func (g *generatedReader) Read(p []byte) (int, error) {
	if g.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > g.remaining {
		n = int(g.remaining)
	}
	var word [8]byte
	for i := 0; i < n; i += 8 {
		g.state ^= g.state << 13
		g.state ^= g.state >> 7
		g.state ^= g.state << 17
		binary.LittleEndian.PutUint64(word[:], g.state*0x2545F4914F6CDD1D)
		copy(p[i:min(i+8, n)], word[:])
	}
	g.remaining -= int64(n)
	return n, nil
}

// failingReader fails after handing over prefix bytes.
type failingReader struct {
	prefix []byte
	err    error
	pos    int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.pos >= len(f.prefix) {
		return 0, f.err
	}
	n := copy(p, f.prefix[f.pos:])
	f.pos += n
	return n, nil
}

// fineConfig is the default parameter table scaled down by 256.
//
// The shift property and the size distribution are statements about a
// population of chunks, and a population needs hundreds of them. At the 1 MiB
// default average that means a hundred-megabyte input in every such test, which
// under -race is minutes rather than seconds. Config exists as a parameter for
// exactly this reason: the arithmetic under test is identical at 4 KiB, and the
// defaults get their own pass over a genuinely large input where it matters.
var fineConfig = Config{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}

func repetitive(n int) []byte {
	return bytes.Repeat([]byte("heyarr heyarr heyarr "), n/21+1)[:n]
}
