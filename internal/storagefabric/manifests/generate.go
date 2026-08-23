package manifests

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
)

// ErrWholeObjectMismatch is bytes that were read as one blob and hash to
// another.
//
// It is not [ErrDigestMismatch]: that one is a manifest that disagrees with
// its own chunks, which is a manifest that was damaged in storage or in
// transit. This is a manifest that would have been internally perfect and
// would have described the WRONG BYTES — every chunk digest correct for what
// was on the disk, and what was on the disk no longer the blob it is stored
// under.
var ErrWholeObjectMismatch = errors.New("manifests: the bytes do not hash to the blob they were read as")

// WholeObjectMismatch is the detail behind [ErrWholeObjectMismatch]: what the
// bytes were stored as, what they actually hash to, and how many were read.
//
// Typed for the same reason the CAS's own Corruption is: the caller's next move is to
// report corruption on the existing path (ADR-0018), and it needs the actual
// digest to record rather than a formatted string to re-parse.
type WholeObjectMismatch struct {
	// Expected is the blob's own name, which is also the digest its bytes were
	// supposed to have (ADR-0005).
	Expected hashing.Hash
	// Actual is what the bytes hash to now.
	Actual hashing.Hash
	// Size is how many bytes were read before the two were compared.
	Size int64
}

func (m *WholeObjectMismatch) Error() string {
	return fmt.Sprintf("%s: %s hashes to %s over %d bytes",
		ErrWholeObjectMismatch.Error(), m.Expected, m.Actual, m.Size)
}

// Unwrap keeps errors.Is(err, ErrWholeObjectMismatch) true.
func (m *WholeObjectMismatch) Unwrap() error { return ErrWholeObjectMismatch }

// Generate reads a blob's bytes ONCE and returns its manifest.
//
// # One pass, two hashes, and the second one is the point
//
// The chunker digests each chunk as it cuts it; the same bytes go through a
// whole-object hasher on the way past. At EOF the whole-object digest is
// compared with the blob's own name, and a mismatch returns
// [WholeObjectMismatch] and NO manifest.
//
// That check is what makes generation self-verifying, and it is not
// defensiveness. A manifest is what a later repair or a resumed transfer
// reassembles from (ADR-0035, ADR-0036), so a manifest built from bytes nobody
// checked is a set of individually valid chunks describing a file that is not
// the one it is named after — and every consumer downstream would then verify
// its reassembly against exactly the digests this function wrote down, and
// agree. The corruption would be laundered into a manifest and become
// self-consistent.
//
// Reading the blob and hashing it whole is the cost that check would otherwise
// have: this handler is already reading every byte, so the whole-object hash is
// one extra pass over data already in cache and the only honest moment to take
// it. There is no cheaper time to notice.
//
// # Deterministic
//
// Nothing here reads a clock, a random source or a counter. `at` is a
// parameter, and it is excluded from [Manifest.ComputeDigest] deliberately, so
// two runs over the same bytes with the same parameters produce the same
// manifest digest — which is the property Invariant 9's idempotence is
// asserted through.
//
// # Cancellation leaves nothing
//
// It returns an error and no manifest. Nothing is written here at all — the
// caller does the single write, after this returns — so a cancelled generation
// cannot leave a partial manifest behind, by construction rather than by
// clean-up.
func Generate(
	ctx context.Context, r io.Reader, blob hashing.Hash, params chunking.Config, at time.Time,
) (Manifest, error) {
	if blob.IsZero() {
		return Manifest{}, fmt.Errorf("%w: a manifest describes a blob, and this one names none",
			ErrMalformed)
	}
	whole := hashing.New()
	chunker, err := chunking.New(io.TeeReader(r, whole), params)
	if err != nil {
		return Manifest{}, err
	}

	var (
		chunks []chunking.Chunk
		read   int64
	)
	for {
		if err := ctx.Err(); err != nil {
			// Between chunks rather than between bytes: a chunk is at most
			// Max, so the granularity is a few megabytes of reading and the
			// alternative is a context check in the chunker's inner loop.
			return Manifest{}, err
		}
		chunk, err := chunker.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, err
		}
		chunks = append(chunks, chunk)
		read += chunk.Length
	}

	// AFTER the stream is drained, so the hasher has seen every byte the
	// chunker read ahead of the last boundary as well.
	if actual := whole.Sum(); !actual.Equal(blob) {
		return Manifest{}, &WholeObjectMismatch{Expected: blob, Actual: actual, Size: read}
	}
	return Build(blob, params, chunks, at)
}

// LocalChunks is the manifest read as this node's index entries: these bytes,
// held here, at these offsets inside this blob.
//
// A function rather than something Generate returns as a second value, because
// the index is derivable from the manifest and a second return value would be
// a second thing that could disagree with the first.
func LocalChunks(m Manifest) []LocalChunk {
	out := make([]LocalChunk, 0, len(m.Chunks))
	for _, c := range m.Chunks {
		out = append(out, LocalChunk{
			Digest:   c.Digest,
			BlobHash: m.BlobHash,
			Offset:   c.Offset,
			Length:   c.Length,
		})
	}
	return out
}
