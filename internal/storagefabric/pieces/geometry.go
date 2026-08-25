package pieces

import (
	"errors"
	"fmt"
)

// Errors this package returns.
var (
	// ErrEmptyBlob is a zero-length target. There is nothing to exchange and
	// the caller should not have started a session.
	ErrEmptyBlob = errors.New("pieces: a zero-length blob has no pieces")
	// ErrOutOfRange is a piece index that does not exist for this geometry.
	ErrOutOfRange = errors.New("pieces: no such piece")
)

// Geometry is how one blob is divided for exchange.
//
// # Derived, never agreed
//
// Two peers compute this independently from facts they both already have — the
// blob's size — and get the same answer. Nothing is negotiated, nothing is
// stored, and no peer can influence another's view of what a piece is by
// claiming anything. That matters more than it looks: a negotiated geometry is
// a thing a lying peer can move, and every piece hash a destination checks is
// only meaningful relative to the division that produced it.
//
// # Not durable
//
// ADR-0041: a piece table is a transport detail with a session's lifetime and
// is never an identity. This value is recomputed whenever it is needed and
// written nowhere. If it ever needs storing, that is the signal something has
// started treating a piece as a name for bytes, which is what the whole-object
// digest is for.
type Geometry struct {
	// Size is the blob's total length in bytes.
	Size int64
	// PieceLength is every piece's length except the last.
	PieceLength int64
}

// Piece length bounds.
//
// # Why a range rather than a constant
//
// A fixed piece length is wrong at both ends of the size distribution this
// fabric holds. At 256 KiB a 60 GB remux is 245,000 pieces, and the bookkeeping
// — availability, request tracking, per-piece hashes — stops being free. At
// 16 MiB a 4 MiB subtitle sidecar is one piece, which makes exchange pointless
// because there is nothing to divide between peers.
//
// So the length scales with the blob, clamped at both ends, and the clamps are
// the interesting part rather than the formula.
const (
	// MinPieceLength is the floor. Below this the per-piece overhead dominates
	// the bytes moved, and a swarm spends its time talking about pieces rather
	// than sending them.
	MinPieceLength = 256 << 10 // 256 KiB
	// MaxPieceLength is the ceiling. Above this a failed piece costs too much
	// to re-fetch, and a peer that is slow on one piece stalls a
	// disproportionate share of the transfer.
	MaxPieceLength = 16 << 20 // 16 MiB
	// targetPieceCount is what the scaling aims for between the clamps.
	//
	// A thousand-odd pieces is enough that work divides evenly between a
	// handful of peers and a single slow source is not a large fraction, and
	// few enough that availability is a small message rather than a structure
	// worth compressing.
	targetPieceCount = 1024
)

// For returns the geometry for a blob of this size.
//
// # The piece length is a power of two, and that is not decoration
//
// It makes a piece boundary computable with a shift, makes every piece except
// the last identically sized under any implementation that gets the same size,
// and removes a whole family of rounding disagreements between two peers that
// must divide the same blob identically. Two implementations that agree on
// "size/1024 rounded somehow" do not necessarily agree; two that agree on "the
// power of two nearest that, clamped" do.
func For(size int64) (Geometry, error) {
	if size <= 0 {
		return Geometry{}, fmt.Errorf("%w: %d", ErrEmptyBlob, size)
	}

	length := int64(MinPieceLength)
	for length < MaxPieceLength && size/length > targetPieceCount {
		length <<= 1
	}
	return Geometry{Size: size, PieceLength: length}, nil
}

// Count is how many pieces this blob has.
//
// The last piece is short whenever the size is not a multiple of the piece
// length, which is the ordinary case. It is a piece like any other: it is
// requested, verified and counted the same way, and code that special-cases it
// is code that will get the boundary wrong.
func (g Geometry) Count() int {
	if g.PieceLength <= 0 || g.Size <= 0 {
		return 0
	}
	return int((g.Size + g.PieceLength - 1) / g.PieceLength)
}

// Range is the byte range of one piece, as an offset and a length.
//
// Returned as a range rather than as bytes because that is what the transport
// asks a peer for: §28's ranged read is the byte-carrying half, and this
// package never touches the bytes themselves.
func (g Geometry) Range(index int) (offset, length int64, err error) {
	if index < 0 || index >= g.Count() {
		return 0, 0, fmt.Errorf("%w: %d of %d", ErrOutOfRange, index, g.Count())
	}
	offset = int64(index) * g.PieceLength
	length = g.PieceLength
	if remaining := g.Size - offset; remaining < length {
		length = remaining
	}
	return offset, length, nil
}

// IndexAt is the piece containing a byte offset.
//
// Used when a partial blob is described by how many verified bytes it holds —
// M5's resumption keeps a verified PREFIX (ADR-0035), and turning a prefix
// length into "which pieces do I have" is exactly this.
func (g Geometry) IndexAt(offset int64) (int, error) {
	if offset < 0 || offset >= g.Size || g.PieceLength <= 0 {
		return 0, fmt.Errorf("%w: offset %d in %d", ErrOutOfRange, offset, g.Size)
	}
	return int(offset / g.PieceLength), nil
}

// Complete reports whether every piece is present.
func (g Geometry) Complete(have Availability) bool {
	return have.Count() == g.Count() && g.Count() > 0
}
