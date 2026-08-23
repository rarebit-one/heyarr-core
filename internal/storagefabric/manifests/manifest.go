package manifests

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
)

// AlgorithmFastCDC names the chunker that produced a manifest.
//
// The algorithm is stored with every manifest rather than assumed, because a manifest computed
// under a different algorithm — or different parameters — describes the same
// bytes and shares none of the same boundaries. A peer that upgrades its
// defaults has to be able to tell that what it is holding is not comparable
// with what it would compute now; without the parameters that is unknowable,
// and the failure is silent rather than loud (a diff against an incomparable
// manifest reports nothing reusable, which looks exactly like a blob that
// changed).
const AlgorithmFastCDC = "fastcdc"

// Errors a manifest can be wrong in.
var (
	// ErrMalformed is a manifest that does not describe a byte sequence: a
	// gap, an overlap, a chunk that starts before zero, a count that does not
	// match the rows.
	ErrMalformed = errors.New("manifests: malformed manifest")
	// ErrDigestMismatch is a manifest whose recorded digest does not match the
	// chunk sequence it arrived with. That is a tampered or truncated
	// manifest, and it is caught before anything reassembles from it.
	ErrDigestMismatch = errors.New("manifests: manifest digest does not match its chunks")
)

// Manifest is one blob's chunk sequence.
//
// # It is keyed by the blob, and it is never a key
//
// BlobHash is the ADR-0005 whole-object identity of the bytes this manifest
// describes, and it is the only handle a manifest is ever reachable by. Digest
// is a digest OF THIS MANIFEST, for the manifest's own integrity — a
// destination handed one is entitled to check it arrived intact. ADR-0034 is
// explicit that that digest "names the manifest. It is not an alias for the
// blob ... and nothing may resolve it to bytes."
//
// # Chunks is a SEQUENCE
//
// Not a set. The order is the data: a set of individually valid chunks
// assembled in the wrong order is a set of valid chunks and the wrong file,
// and only the whole-object hash detects it. Every path that reads a manifest
// preserves index order, and [Manifest.Validate] refuses one that does not.
type Manifest struct {
	BlobHash    hashing.Hash
	Algorithm   string
	Params      chunking.Config
	CoveredSize int64
	Digest      hashing.Hash
	GeneratedAt time.Time
	Chunks      []chunking.Chunk
}

// ChunkCount is how many chunks the manifest holds.
//
// A method rather than a stored field so it cannot disagree with Chunks. The
// database stores it as a column, and [Manifest.Validate] is what makes the
// two agree on the way back in.
func (m Manifest) ChunkCount() int { return len(m.Chunks) }

// Build assembles a manifest from a chunker's output and computes its digest.
//
// It validates before it digests, so a malformed manifest can never acquire a
// digest that makes it look intact.
func Build(
	blobHash hashing.Hash, params chunking.Config, chunks []chunking.Chunk, at time.Time,
) (Manifest, error) {
	m := Manifest{
		BlobHash:    blobHash,
		Algorithm:   AlgorithmFastCDC,
		Params:      params,
		Chunks:      chunks,
		GeneratedAt: at.UTC(),
	}
	if len(chunks) > 0 {
		m.CoveredSize = chunks[len(chunks)-1].End()
	}
	if err := m.validateShape(); err != nil {
		return Manifest{}, err
	}
	m.Digest = m.ComputeDigest()
	return m, nil
}

// ComputeDigest is the manifest's own content address.
//
// The encoding is canonical and unambiguous: every field is length-prefixed or
// newline-terminated, and the chunk digests go in **in index order**. That
// ordering is not incidental — it is what makes the digest detect a reordered
// manifest at all. A digest over an order-insensitive encoding would accept the
// permutation that reassembles the wrong file, which is the one fault the
// whole-object hash is otherwise alone in catching (ADR-0034).
//
// The blob's own hash is bound in, so a manifest cannot be lifted from one blob
// and presented as another's.
func (m Manifest) ComputeDigest() hashing.Hash {
	h := hashing.New()
	write := func(parts ...string) {
		for _, p := range parts {
			// A length prefix rather than a separator: without it,
			// ("ab", "c") and ("a", "bc") hash the same, and a manifest
			// digest that can be collided by re-splitting its own fields is
			// not an integrity check.
			_, _ = h.Write([]byte(strconv.Itoa(len(p))))
			_, _ = h.Write([]byte(":"))
			_, _ = h.Write([]byte(p))
		}
	}
	num := func(n int64) { write(strconv.FormatInt(n, 10)) }

	write("heyarr.chunk-manifest.v1")
	write(m.BlobHash.String())
	write(m.Algorithm)
	num(int64(m.Params.Min))
	num(int64(m.Params.Avg))
	num(int64(m.Params.Max))
	num(m.CoveredSize)
	num(int64(len(m.Chunks)))
	for _, c := range m.Chunks {
		num(c.Offset)
		num(c.Length)
		write(c.Digest.String())
	}
	return h.Sum()
}

// Validate checks the shape AND the digest.
//
// This is what a read does. The digest is recomputed rather than trusted, so a
// tampered `manifest_chunks` row — a swapped digest, a changed length, two rows
// exchanged — is detected here rather than at reassembly, or worse, not at all.
func (m Manifest) Validate() error {
	if err := m.validateShape(); err != nil {
		return err
	}
	if got := m.ComputeDigest(); !got.Equal(m.Digest) {
		return fmt.Errorf("%w: manifest for %s records %s, its chunks hash to %s",
			ErrDigestMismatch, m.BlobHash, m.Digest, got)
	}
	return nil
}

// validateShape is the arithmetic: contiguous from zero, no gaps, no overlaps,
// no empty chunks, and a covered size that is the end of the last chunk.
func (m Manifest) validateShape() error {
	if m.BlobHash.IsZero() {
		return fmt.Errorf("%w: a manifest describes a blob, and this one names none", ErrMalformed)
	}
	if m.Algorithm == "" {
		return fmt.Errorf("%w: manifest for %s names no chunker", ErrMalformed, m.BlobHash)
	}
	if err := m.Params.Validate(); err != nil {
		return fmt.Errorf("%w: manifest for %s: %w", ErrMalformed, m.BlobHash, err)
	}
	var want int64
	for i, c := range m.Chunks {
		if c.Offset != want {
			return fmt.Errorf(
				"%w: manifest for %s: chunk %d starts at %d, the previous one ends at %d — "+
					"a gap or an overlap, and either reassembles the wrong bytes",
				ErrMalformed, m.BlobHash, i, c.Offset, want)
		}
		if c.Length <= 0 {
			return fmt.Errorf("%w: manifest for %s: chunk %d has length %d",
				ErrMalformed, m.BlobHash, i, c.Length)
		}
		if c.Digest.IsZero() {
			return fmt.Errorf("%w: manifest for %s: chunk %d has no digest",
				ErrMalformed, m.BlobHash, i)
		}
		want = c.End()
	}
	if m.CoveredSize != want {
		return fmt.Errorf(
			"%w: manifest for %s says it covers %d bytes, its chunks cover %d",
			ErrMalformed, m.BlobHash, m.CoveredSize, want)
	}
	return nil
}

// Covers reports whether this manifest accounts for every byte of a blob of
// the given size.
//
// Separate from Validate because a manifest is internally consistent without
// being complete, and "complete for THIS blob" is a question only a caller
// holding the blob's size can ask.
func (m Manifest) Covers(blobSize int64) bool { return m.CoveredSize == blobSize }

// ComparableWith reports whether two manifests were produced under the same
// chunker settings, and so whether their chunk lists can meaningfully be
// diffed. This is what the stored parameters are for.
func (m Manifest) ComparableWith(other Manifest) bool {
	return m.Algorithm == other.Algorithm && m.Params == other.Params
}
