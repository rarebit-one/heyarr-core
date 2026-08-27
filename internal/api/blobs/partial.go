package blobs

import (
	"context"
	"errors"
	"io"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// ErrTransferGone is returned by a partial read when the transfer feeding the
// blob ended without producing the byte the reader is waiting for, and the blob
// did not become whole. It is distinct from a cancelled request (the context
// error) because the two want different handling: a cancel is the client
// leaving, this is the content never arriving.
var ErrTransferGone = errors.New("blobs: the transfer feeding this partial ended before the range landed")

// PartialSource serves a blob that is still arriving, in BYTE terms. It is
// deliberately piece-agnostic: this package serves byte ranges and says nothing
// about what they mean (ADR-0013, §27), and a piece is a byte range the range
// machinery already handles. The translation from a piece-availability record
// into "which bytes are here" lives behind this interface, where the controller
// already knows what a piece is (peersurface.go) — so the blob-serving code
// stays free of piece awareness, which webseed_test.go's guard enforces.
//
// It is optional on the handler, and passed only by the CLIENT wiring: the peer
// content route leaves it nil and keeps its untouched whole-blob contract
// (ADR-0042).
type PartialSource interface {
	// ArrivingSize is the blob's whole logical length while a transfer is
	// assembling it. inflight=false means no transfer is in flight — the caller
	// should answer 404 exactly as it did before partials existed.
	ArrivingSize(ctx context.Context, blob hashing.Hash) (size int64, inflight bool, err error)

	// Available reports, for a byte offset in a blob still arriving, whether that
	// byte has landed and how far a read from it may run:
	//
	//   inflight=false           the transfer ended; nothing more is coming
	//   inflight=true, ok=false  arriving, this byte has not landed yet — block
	//   inflight=true, ok=true   landed; contiguously readable up to `until`
	//
	// `until` is an exclusive upper bound in blob-absolute bytes. The caller must
	// call this before every read and bound the read to `until`, which is the one
	// place believing the record wrongly would ship bytes that have not landed.
	Available(ctx context.Context, blob hashing.Hash, off int64) (until int64, ok, inflight bool, err error)

	// ReadPartialAt reads landed bytes from the still-assembling blob. The caller
	// MUST have confirmed the range via Available first — this reads whatever is
	// on disk, holes included.
	ReadPartialAt(blob hashing.Hash, b []byte, off int64) (int, error)
}

// partialBlobReader presents a blob that is still assembling as a seekable
// stream whose Read BLOCKS until the requested bytes have landed. It is what the
// client blob route hands http.ServeContent so a player receives ordinary
// HTTP/HLS (a 200, or a 206 with the whole logical length) over content that has
// not finished arriving — §33, and the first deliverable of §84's Milestone 10.
//
// # The one invariant it exists to hold
//
// It returns only bytes PartialSource.Available says have landed, bounded to the
// `until` that call reports. A gap is WAITED ON, never read — because a hole
// reads back as zeroes indistinguishable from received data (ADR-0043), and here
// those zeroes would go to a client as if they were the content. This is the
// client-side mirror of the peer surface's ReadPiece, and the same place
// ADR-0042/0043 name as the one where believing a record wrongly ships bad bytes
// to somebody else rather than failing locally.
type partialBlobReader struct {
	ctx   context.Context
	store cas.Store     // for the transparent transition to a completed replica
	src   PartialSource // the byte-level availability of the still-assembling blob
	// wait blocks until it is worth re-consulting availability, or until ctx
	// ends. It is the whole of the block-then-serve: a byte lands in another ROLE
	// (the worker running the transfer, §4/invariant 4), so the only role-legal
	// signal is what that worker persists — this waits, then re-reads it.
	// Injected so a test drives it deterministically instead of sleeping.
	wait func(context.Context) error

	blob hashing.Hash
	// size is the blob's whole logical length, captured once at construction. A
	// blob's size is fixed before the transfer starts, so it never changes under
	// the reader; only which bytes have landed does, and that is re-read per gap.
	size int64
	pos  int64

	// whole is the finished blob's reader, opened lazily once the transfer
	// completes. From then on every read comes from here — the transparent
	// transition to a complete local replica (§84).
	whole cas.ReadSeekCloser
}

// Seek implements io.Seeker against the blob's whole logical length, which is
// what makes ServeContent able to answer a Range over a partial: the length it
// advertises is the blob's true size, not the high-water mark of what has landed
// (ADR-0043). ServeContent seeks to the end to size the response and back to the
// range start; both are arithmetic here and touch no bytes.
func (r *partialBlobReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, errors.New("blobs: invalid seek whence")
	}
	if abs < 0 {
		return 0, errors.New("blobs: negative seek position")
	}
	r.pos = abs
	return abs, nil
}

// Read serves the next bytes at the current position, blocking until they land.
//
// The order of the checks is load-bearing. Completion is tested FIRST, so the
// race between "the last bytes landed" and "the transfer published the whole
// blob and reaped its staging file" always resolves toward serving the finished
// blob, never toward calling the range abandoned a moment before it was there.
func (r *partialBlobReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}

		// 1. Completed? Read the rest from the finished blob (§84's transparent
		//    transition). Checked before availability so completion never looks
		//    like abandonment.
		if r.whole == nil {
			rsc, err := r.openWholeIfReady()
			if err != nil {
				return 0, err
			}
			r.whole = rsc
		}
		if r.whole != nil {
			if _, err := r.whole.Seek(r.pos, io.SeekStart); err != nil {
				return 0, err
			}
			n, err := r.whole.Read(clip(p, r.size-r.pos))
			r.pos += int64(n)
			return n, err
		}

		// 2. Landed? Serve up to `until` — never past the bound availability just
		//    confirmed.
		until, ok, inflight, err := r.src.Available(r.ctx, r.blob, r.pos)
		if err != nil {
			return 0, err
		}
		if !inflight {
			// The transfer ended without this byte, and the blob is not whole
			// (checked above). A whole-blob pull that fails does the same — the
			// bytes are simply not coming.
			return 0, ErrTransferGone
		}
		if ok {
			end := until
			if r.size < end {
				end = r.size
			}
			n, rerr := r.src.ReadPartialAt(r.blob, clip(p, end-r.pos), r.pos)
			r.pos += int64(n)
			if n > 0 {
				return n, nil
			}
			// A zero-length read with io.EOF here means the staging file is
			// shorter than availability claimed — a torn write the whole-object
			// hash catches at Publish, but not a reason to spin. Treat it as the
			// byte not being readable yet and wait.
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return 0, rerr
			}
		}

		// 3. The byte at pos has not landed. Wait, then re-consult availability.
		if err := r.wait(r.ctx); err != nil {
			return 0, err
		}
	}
}

// openWholeIfReady returns the finished blob's reader if the transfer has
// completed and published it, or (nil, nil) while it is still in flight. Only an
// unexpected store error stops the read; a plain not-found is the ordinary
// "still arriving" answer.
func (r *partialBlobReader) openWholeIfReady() (cas.ReadSeekCloser, error) {
	rsc, _, err := r.store.Open(r.ctx, r.blob)
	switch {
	case err == nil:
		return rsc, nil
	case errors.Is(err, cas.ErrNotFound):
		return nil, nil
	default:
		return nil, err
	}
}

// Close releases the finished-blob reader if one was opened. The partial read
// itself opens and closes a file per call and holds nothing to release.
func (r *partialBlobReader) Close() error {
	if r.whole != nil {
		return r.whole.Close()
	}
	return nil
}

// clip caps p at n bytes, where n is how many remain before some boundary — the
// end of the landed run, or the end of the blob. It never grows p.
func clip(p []byte, n int64) []byte {
	if n < int64(len(p)) {
		return p[:n]
	}
	return p
}
