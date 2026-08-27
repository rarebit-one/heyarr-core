package controller

import (
	"context"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/pieces"
)

// piecePartialSource adapts the CAS's opaque piece-availability record into the
// byte-level [blobs.PartialSource] the client blob route needs to serve a blob
// that is still arriving (§33, §84, ADR-0044).
//
// Piece geometry is decoded HERE, in the controller, which already knows what a
// piece is (peersurface.go's ReadPiece does the same decode on the peer side).
// That is deliberate: the blob-serving package speaks byte ranges and says
// nothing about what they mean (ADR-0013, §27), and webseed_test.go's guard
// enforces that it imports no piece machinery. This adapter is where the
// translation from "which pieces landed" to "which bytes are here" lives, on the
// side of the boundary that is allowed to know.
//
// The record it reads is a HINT (ADR-0043): it is consulted before every read
// and never trusted as evidence, and the whole-object digest verified at Publish
// is the only identity. A record that overstates costs a blocked read that
// resolves when the bytes truly land, never a wrong byte on the wire.
// partialStore is what the adapter needs of the CAS: the read side of the
// availability record and the partial bytes (which the peer surface also uses),
// plus the write side of the playback-window hint. Asserted at the wiring site,
// not added to any shared Store, for the reason ADR-0013 and pieceProgressStore
// both give — a capability only the client route needs should not become a
// method every store must grow.
type partialStore interface {
	LoadPieceProgress(blob hashing.Hash) (string, error)
	ReadPartialAt(blob hashing.Hash, b []byte, off int64) (int, error)
	SavePlayhead(blob hashing.Hash, off int64) error
}

type piecePartialSource struct {
	store partialStore
	log   *slog.Logger
}

var _ blobs.PartialSource = piecePartialSource{}

// ArrivingSize decodes the blob's whole logical length from the availability
// record, and reports whether a transfer is in flight at all. A zero digest, an
// absent record and a record too corrupt to parse all report "not in flight" —
// the route answers 404 for each, never 500, matching what it did before
// partials existed.
func (s piecePartialSource) ArrivingSize(_ context.Context, blob hashing.Hash) (int64, bool, error) {
	if blob.IsZero() {
		return 0, false, nil
	}
	encoded, err := s.store.LoadPieceProgress(blob)
	if err != nil {
		return 0, false, err
	}
	if encoded == "" {
		return 0, false, nil
	}
	g, _, err := pieces.Decode(encoded)
	if err != nil {
		s.log.Error("decoding piece progress failed", "blob", blob.String(), "error", err)
		return 0, false, nil
	}
	return g.Size, true, nil
}

// Available translates the piece bitset into blob-absolute byte bounds for one
// offset: whether the byte at off has landed, and the exclusive offset up to
// which a read from it may run — the end of the landed piece that contains it.
func (s piecePartialSource) Available(
	_ context.Context, blob hashing.Hash, off int64,
) (until int64, ok, inflight bool, err error) {
	if blob.IsZero() {
		return 0, false, false, nil
	}
	encoded, lerr := s.store.LoadPieceProgress(blob)
	if lerr != nil {
		return 0, false, false, lerr
	}
	if encoded == "" {
		return 0, false, false, nil // not in flight
	}
	g, have, derr := pieces.Decode(encoded)
	if derr != nil {
		// A record too corrupt to parse is treated as the transfer being gone,
		// so the reader stops rather than spinning on bytes it cannot place.
		s.log.Error("decoding piece progress failed", "blob", blob.String(), "error", derr)
		return 0, false, false, nil
	}
	idx, ierr := g.IndexAt(off)
	if ierr != nil {
		// The reader bounds off below the blob size, so this is a geometry that
		// disagrees with the size it reported — treat it as gone rather than
		// serve into it.
		return 0, false, false, nil
	}
	if !have.Has(idx) {
		return 0, false, true, nil // in flight, this byte has not landed yet
	}
	pieceOff, length, rerr := g.Range(idx)
	if rerr != nil {
		return 0, false, false, nil
	}
	return pieceOff + length, true, true, nil
}

// ReadPartialAt reads landed bytes from the still-assembling blob. The caller
// must have confirmed the range via Available first.
func (s piecePartialSource) ReadPartialAt(blob hashing.Hash, b []byte, off int64) (int, error) {
	return s.store.ReadPartialAt(blob, b, off)
}

// SetPlayhead records where a consumer is reading so the transfer can prioritise
// the pieces near it (§33, §84). The offset is a byte fact the reader knows; the
// transfer turns it into a piece index on its own side. It writes straight
// through to the CAS sidecar the transfer reads — no piece decoding here, because
// a playhead has none.
func (s piecePartialSource) SetPlayhead(_ context.Context, blob hashing.Hash, off int64) error {
	return s.store.SavePlayhead(blob, off)
}
