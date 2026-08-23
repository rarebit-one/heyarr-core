package manifests_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

var at = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func hash(t *testing.T, n int) hashing.Hash {
	t.Helper()
	h, err := hashing.Parse(fmt.Sprintf("blake3:%064x", n))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func seq(t *testing.T, lengths ...int64) []chunking.Chunk {
	t.Helper()
	var out []chunking.Chunk
	var off int64
	for i, n := range lengths {
		out = append(out, chunking.Chunk{Offset: off, Length: n, Digest: hash(t, i+1)})
		off += n
	}
	return out
}

// THE property the manifest digest exists for.
//
// ADR-0034: "a set of individually valid chunks assembled in the wrong order
// is a set of valid chunks and the wrong file." A digest computed over an
// order-insensitive encoding — a sum, a sorted set, an XOR — would accept the
// permutation that reassembles the wrong bytes, and the whole-object hash at
// the end of a completed transfer would be the only thing that noticed.
func TestTheManifestDigestChangesWhenTheChunksAreReordered(t *testing.T) {
	blob := hash(t, 0xb10b)
	forward := seq(t, 100, 200, 300)

	// The same three chunks, in the other order, still contiguous from zero.
	reversed := []chunking.Chunk{
		{Offset: 0, Length: 300, Digest: forward[2].Digest},
		{Offset: 300, Length: 200, Digest: forward[1].Digest},
		{Offset: 500, Length: 100, Digest: forward[0].Digest},
	}

	a, err := manifests.Build(blob, chunking.DefaultConfig(), forward, at)
	if err != nil {
		t.Fatal(err)
	}
	b, err := manifests.Build(blob, chunking.DefaultConfig(), reversed, at)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest.Equal(b.Digest) {
		t.Fatal("two manifests over the same chunks in different orders hash the same — " +
			"the digest is order-insensitive and cannot detect the one fault it is for")
	}
}

// The digest binds the blob, so a manifest cannot be lifted from one blob and
// presented as another's.
func TestTheManifestDigestBindsTheBlobItDescribes(t *testing.T) {
	chunks := seq(t, 100, 200)
	a, err := manifests.Build(hash(t, 1), chunking.DefaultConfig(), chunks, at)
	if err != nil {
		t.Fatal(err)
	}
	b, err := manifests.Build(hash(t, 2), chunking.DefaultConfig(), chunks, at)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest.Equal(b.Digest) {
		t.Error("two blobs with identical chunk lists produced one manifest digest")
	}
}

// The chunker parameters are inside the digest, because a manifest computed
// under other settings describes the same bytes and shares no boundaries.
func TestTheManifestDigestCoversTheChunkerParameters(t *testing.T) {
	blob := hash(t, 3)
	chunks := seq(t, 100, 200)
	a, err := manifests.Build(blob, chunking.DefaultConfig(), chunks, at)
	if err != nil {
		t.Fatal(err)
	}
	other := chunking.Config{Min: 64 << 10, Avg: 256 << 10, Max: 1 << 20}
	b, err := manifests.Build(blob, other, chunks, at)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest.Equal(b.Digest) {
		t.Error("the parameters are not covered by the digest")
	}
	if a.ComparableWith(b) {
		t.Error("manifests at different parameters reported themselves comparable")
	}
	if !a.ComparableWith(a) {
		t.Error("a manifest is not comparable with itself")
	}
}

// Validate is what a read runs, and it is where a tampered manifest is caught.
func TestValidateRejectsAManifestThatDoesNotDescribeAByteSequence(t *testing.T) {
	blob := hash(t, 4)
	good, err := manifests.Build(blob, chunking.DefaultConfig(), seq(t, 100, 200), at)
	if err != nil {
		t.Fatal(err)
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a manifest straight out of Build failed validation: %v", err)
	}

	tests := []struct {
		name    string
		breakIt func(m *manifests.Manifest)
		want    error
	}{
		{"a gap between chunks", func(m *manifests.Manifest) {
			m.Chunks[1].Offset += 10
		}, manifests.ErrMalformed},
		{"an overlap", func(m *manifests.Manifest) {
			m.Chunks[1].Offset -= 10
		}, manifests.ErrMalformed},
		{"a zero-length chunk", func(m *manifests.Manifest) {
			m.Chunks[1].Length = 0
		}, manifests.ErrMalformed},
		{"a covered size that is not the end of the last chunk", func(m *manifests.Manifest) {
			m.CoveredSize += 1
		}, manifests.ErrMalformed},
		{"a swapped chunk digest, everything else intact", func(m *manifests.Manifest) {
			m.Chunks[1].Digest = hash(t, 0xdead)
		}, manifests.ErrDigestMismatch},
		{"two chunks of equal length exchanged", func(m *manifests.Manifest) {
			m.Chunks[0].Digest, m.Chunks[1].Digest = m.Chunks[1].Digest, m.Chunks[0].Digest
		}, manifests.ErrDigestMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := good
			m.Chunks = append([]chunking.Chunk(nil), good.Chunks...)
			tt.breakIt(&m)
			err := m.Validate()
			if err == nil {
				t.Fatal("validation passed")
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

// An exchange of two EQUAL-length chunks is the sharpest reordering case: every
// offset stays contiguous, every length is unchanged, and only the digest
// notices.
func TestExchangingTwoEqualChunksIsCaughtByTheDigestAlone(t *testing.T) {
	blob := hash(t, 5)
	m, err := manifests.Build(blob, chunking.DefaultConfig(), seq(t, 128, 128), at)
	if err != nil {
		t.Fatal(err)
	}
	m.Chunks[0].Digest, m.Chunks[1].Digest = m.Chunks[1].Digest, m.Chunks[0].Digest

	// The shape is still perfect — contiguous, non-empty, covered.
	var off int64
	for _, c := range m.Chunks {
		if c.Offset != off {
			t.Fatal("setup: the exchange disturbed the offsets")
		}
		off = c.End()
	}
	if !errors.Is(m.Validate(), manifests.ErrDigestMismatch) {
		t.Fatal("a permutation of two equal-length chunks was not detected")
	}
}

// An empty blob has an empty manifest, and it is valid. The alternative is a
// special case at every call site.
func TestAnEmptyBlobHasAnEmptyManifest(t *testing.T) {
	m, err := manifests.Build(hash(t, 6), chunking.DefaultConfig(), nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if m.ChunkCount() != 0 || m.CoveredSize != 0 {
		t.Fatalf("count=%d covered=%d", m.ChunkCount(), m.CoveredSize)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if !m.Covers(0) {
		t.Error("an empty manifest does not cover an empty blob")
	}
}

// The three states are compared by equality, and the names are chosen so that
// none contains another — the assert_contains failure mode this repo has
// already shipped once.
func TestTheStatesAreDistinctAndNoneContainsAnother(t *testing.T) {
	all := []manifests.State{
		manifests.StatePresent, manifests.StateNotRequired, manifests.StateUndecided,
	}
	for i, a := range all {
		if !a.Valid() {
			t.Errorf("%q is not valid", a)
		}
		parsed, err := manifests.ParseState(string(a))
		if err != nil || parsed != a {
			t.Errorf("ParseState(%q) = %q, %v", a, parsed, err)
		}
		for j, b := range all {
			if i == j {
				continue
			}
			if a == b {
				t.Errorf("%q and %q are the same state", a, b)
			}
			if len(a) > len(b) && a[len(a)-len(b):] == b {
				t.Errorf("%q ends with %q — an assert_contains on these would pass wrongly", a, b)
			}
			if len(a) > len(b) && a[:len(b)] == b {
				t.Errorf("%q starts with %q — an assert_contains on these would pass wrongly", a, b)
			}
		}
	}
	if _, err := manifests.ParseState("chunked"); err == nil {
		t.Error("an unknown state parsed rather than being refused")
	}
	if _, err := manifests.ParseState(""); err == nil {
		t.Error("the empty string parsed as a state; a missing value must not default")
	}

	// The compatibility boolean is true for exactly one state.
	if !manifests.StatePresent.HasManifest() {
		t.Error("present does not report a manifest")
	}
	if manifests.StateNotRequired.HasManifest() || manifests.StateUndecided.HasManifest() {
		t.Error("a state with no manifest reported one — this is the boolean lying again")
	}
}

// A manifest built from the real chunker round-trips through Validate, so the
// two packages agree about what a chunk sequence is.
func TestAManifestOverRealChunkerOutputValidates(t *testing.T) {
	cfg := chunking.Config{Min: 64, Avg: 256, Max: 1024}
	data := make([]byte, 40_000)
	for i := range data {
		data[i] = byte(i*7 + i/251)
	}
	c, err := chunking.New(bytes.NewReader(data), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var chunks []chunking.Chunk
	for {
		ch, err := c.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, ch)
	}
	whole, _, err := hashing.HashReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifests.Build(whole, cfg, chunks, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if !m.Covers(int64(len(data))) {
		t.Errorf("the manifest covers %d of %d bytes", m.CoveredSize, len(data))
	}
}
