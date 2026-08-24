package cas

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// Resumable staging: bytes that survive an attempt, and are believed by
// nobody (§84, ADR-0035, ADR-0018, M5-06).
//
// # Why the store owns this and the database does not
//
// A partial transfer is a fact about bytes on THIS disk. ADR-0035 puts it in
// the store's private staging area for two reasons that are both operational:
// a store can be moved out from under a database, and a database can be
// restored from a backup taken before those bytes existed. Either way a row
// saying "37% received" describes a file that may not be there, and a file
// that is there is describable by reading it. So there is no row. Migration
// 00030 was reserved for one and is deliberately unused.
//
// # What a partial is allowed to be
//
// It is NOT addressable. It is not in the blob tree, [FS.Has] does not see it,
// [FS.Walk] does not visit it, and nothing can open it by digest. It lives
// under tmp/ with the .part suffix that [FS.TempFiles] already lists and
// [FS.ReapTemp] already deletes, so an abandoned transfer is reaped by age on
// the existing path — no reference count, no negotiation with the job queue,
// no cleverness about a transfer that might come back. A reaped partial costs
// a refetch.
//
// # And what publishing one costs
//
// [Partial.Publish] re-reads the assembled file and hashes it whole before it
// links anything into the blob tree. That is not caution and it is not
// duplicated work that a per-chunk check makes redundant: a set of
// individually valid chunks assembled in the wrong order is a set of valid
// chunks and the wrong file, and only the whole-object hash detects it
// (ADR-0034, invariant 1). The cost is one sequential read of the blob, on the
// machine that already has it, and it is the cost that buys the guarantee
// every other path here also pays for.

// ErrPartialBusy is another transfer in this process already holding a blob's
// staging file.
//
// Two workers assembling one blob into one file would interleave writes and
// each would then fail the whole-object verification — correct, in that
// nothing wrong is published, and a waste of both transfers. Between processes
// the job lease is what keeps this to one (ADR-0008, invariant 9); within a
// process this is.
var ErrPartialBusy = errors.New("cas: another transfer already holds this blob's staging file")

// partialPrefix names a resumable staging file. Distinct from the link-
// prefixed ones so that an operator reading a tmp/ listing can tell an
// interrupted ingest from an interrupted transfer, and so the name is
// derivable from the digest rather than being remembered somewhere.
const partialPrefix = "resume-"

// PartialName is the staging file name a resumable transfer of h uses.
//
// Exported because "an abandoned partial is reapable on the existing path" is
// an assertion somebody has to be able to make, and making it means naming the
// file without reimplementing the layout outside this package (§18).
func PartialName(h hashing.Hash) string { return partialPrefix + h.Hex() + ".part" }

// Partial is a transfer's own staging file: append-only, truncatable, and
// publishable exactly once after the whole-object digest checks out.
//
// It is an interface rather than a struct so that a store which is not a local
// directory can implement one; *filePartial is the local implementation.
type Partial interface {
	// Size is how many bytes are on disk right now. It is progress telemetry
	// and a bound for reads — never a resume offset to be trusted. ADR-0035:
	// "Length is not evidence."
	//
	// For a SPARSELY written partial it is the high-water mark rather than how
	// much is present, so reading it as progress overstates (ADR-0043). A piece
	// transfer's progress is its bitset's count over the geometry's, and code
	// that reaches for Size() there is code that will report a transfer nearly
	// done when its last piece happened to arrive first.
	Size() int64

	// ReadAt reads back what a previous attempt wrote, so the caller can
	// re-hash it against a digest it holds independently. This is the only way
	// bytes leave a partial, and the caller is expected to distrust them.
	ReadAt(p []byte, off int64) (int, error)

	// Append writes r at the current end and returns how many bytes landed.
	//
	// A caller that discovers what it appended does not verify truncates back;
	// that is why this streams rather than taking a buffer the caller could
	// have checked first. Memory stays flat in the chunk size, and a manifest
	// declaring an enormous chunk cannot turn a transfer into an allocation.
	Append(ctx context.Context, r io.Reader) (int64, error)

	// Truncate discards everything from n onwards. Shortening only: a partial
	// never grows except by Append or WriteAt, because a hole is bytes nothing
	// wrote and a file of zeroes reads exactly like a file of received data.
	Truncate(n int64) error

	// WriteAt writes at an arbitrary offset, for a transfer whose bytes do not
	// arrive in order (ADR-0043).
	//
	// # Why this exists when Append's doc argues against holes
	//
	// It argues against UNRECORDED holes, and that is the whole difference. A
	// piece transfer assembles out of order from several peers at once — §23's
	// point — and keeps an availability bitset saying which pieces landed. A
	// hole is then distinguishable from received data, because the bitset says
	// the piece never arrived.
	//
	// The append-only path is unchanged and its warning still applies to it: a
	// sequential resume has no such record, so for that path a hole really is
	// indistinguishable and Append remains the only way to grow.
	//
	// # What stops a wrong bitset producing a wrong blob
	//
	// Publish re-reads the assembled file and hashes it WHOLE against the
	// expected digest. A bitset that lies — by a bug, a torn write, a crash
	// between the write and the record — yields a mismatch and the transfer
	// fails closed. That is what lets the bitset be an optimisation and never
	// evidence (ADR-0034's argument for a manifest, applied here).
	WriteAt(p []byte, off int64) (int, error)

	// Publish verifies the assembled bytes against expected and links them
	// into the blob tree, or quarantines them and reports *Corruption.
	//
	// Whichever happens, the staging file is gone afterwards and the Partial
	// is spent.
	Publish(ctx context.Context, expected hashing.Hash) (Descriptor, error)

	// Discard removes the staging file and releases the blob. For a caller
	// that has decided this partial is not worth keeping; a caller that simply
	// stopped uses Close and lets the reaper have it.
	Discard() error

	// Close releases the blob and leaves the bytes on disk for a later
	// attempt, or for the reaper. Safe to call twice.
	Close() error
}

// OpenPartial opens the resumable staging file for expected, creating it if
// this is the first attempt.
//
// The name is derived from the digest, so a process that was killed mid-
// transfer leaves a file the next attempt finds without being told where it
// is. What that file CONTAINS is a separate question and this method takes no
// position on it: it returns bytes and a length, and every byte of it is
// re-hashed against a manifest before any of it is believed (ADR-0035).
func (s *FS) OpenPartial(_ context.Context, expected hashing.Hash) (Partial, error) {
	if expected.IsZero() {
		return nil, errors.New("cas: refusing to stage a transfer with no expected digest — " +
			"a destination verifies against what it asked for (§21, ADR-0005)")
	}
	key := expected.String()
	if _, loaded := s.inflight.LoadOrStore(key, struct{}{}); loaded {
		return nil, fmt.Errorf("%w: %s", ErrPartialBusy, expected)
	}
	path := filepath.Join(s.root, tmpDir, PartialName(expected))
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		s.inflight.Delete(key)
		return nil, fmt.Errorf("cas: creating the staging directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, tempPerm) // #nosec G304 -- the name is derived from a validated digest, within the configured root
	if err != nil {
		s.inflight.Delete(key)
		return nil, fmt.Errorf("cas: opening the staging file for %s: %w", expected, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		s.inflight.Delete(key)
		return nil, fmt.Errorf("cas: stat of the staging file for %s: %w", expected, err)
	}
	return &filePartial{store: s, blob: expected, path: path, f: f, size: info.Size()}, nil
}

// filePartial is a Partial backed by one file under tmp/.
type filePartial struct {
	store  *FS
	blob   hashing.Hash
	path   string
	f      *os.File
	size   int64
	closed bool
}

var _ Partial = (*filePartial)(nil)

func (p *filePartial) Size() int64 { return p.size }

func (p *filePartial) ReadAt(b []byte, off int64) (int, error) {
	if p.closed {
		return 0, fs.ErrClosed
	}
	return p.f.ReadAt(b, off)
}

func (p *filePartial) Append(ctx context.Context, r io.Reader) (int64, error) {
	if p.closed {
		return 0, fs.ErrClosed
	}
	if _, err := p.f.Seek(p.size, io.SeekStart); err != nil {
		return 0, fmt.Errorf("cas: seeking to the end of the staging file for %s: %w", p.blob, err)
	}
	buf := make([]byte, 1<<20)
	n, err := io.CopyBuffer(p.f, &ctxReader{ctx: ctx, r: r}, buf)
	p.size += n
	if err != nil {
		// The bytes that did land stay. They are inside no verified prefix
		// until something re-hashes them, and the next attempt's prefix scan
		// is what decides whether they were worth keeping.
		return n, fmt.Errorf("cas: appending to the staging file for %s: %w", p.blob, err)
	}
	return n, nil
}

func (p *filePartial) WriteAt(b []byte, off int64) (int, error) {
	if p.closed {
		return 0, fs.ErrClosed
	}
	if off < 0 {
		return 0, fmt.Errorf("cas: refusing to write the staging file for %s at offset %d",
			p.blob, off)
	}
	n, err := p.f.WriteAt(b, off)
	// The high-water mark, not a verified prefix. Size() means something
	// weaker for a sparsely written partial and its doc says so.
	if end := off + int64(n); end > p.size {
		p.size = end
	}
	if err != nil {
		return n, fmt.Errorf("cas: writing the staging file for %s at %d: %w", p.blob, off, err)
	}
	return n, nil
}

func (p *filePartial) Truncate(n int64) error {
	if p.closed {
		return fs.ErrClosed
	}
	if n < 0 || n > p.size {
		return fmt.Errorf("cas: refusing to truncate the staging file for %s from %d to %d — "+
			"a partial only ever shrinks, and extending one would invent bytes nothing received",
			p.blob, p.size, n)
	}
	if err := p.f.Truncate(n); err != nil {
		return fmt.Errorf("cas: truncating the staging file for %s: %w", p.blob, err)
	}
	p.size = n
	return nil
}

// Publish is the one place a resumed or reused transfer becomes a blob.
//
// It re-reads from offset zero. Not from the last chunk boundary, not from a
// running hasher, and not from anything an earlier attempt left behind: the
// whole-object digest is computed here, on this machine, over the bytes about
// to be published, which is invariant 1 stated as control flow. ADR-0035
// forbids the alternative by name — a persisted hasher state is a serialised
// intermediate that nothing verifies, standing in for the one verification
// that has to happen.
func (p *filePartial) Publish(ctx context.Context, expected hashing.Hash) (Descriptor, error) {
	if p.closed {
		return Descriptor{}, fs.ErrClosed
	}
	if !expected.Equal(p.blob) {
		return Descriptor{}, fmt.Errorf(
			"cas: this staging file is for %s and publication was asked for %s", p.blob, expected)
	}
	if err := p.f.Sync(); err != nil {
		return Descriptor{}, fmt.Errorf("cas: syncing the staging file for %s: %w", p.blob, err)
	}
	if _, err := p.f.Seek(0, io.SeekStart); err != nil {
		return Descriptor{}, fmt.Errorf("cas: rewinding the staging file for %s: %w", p.blob, err)
	}
	got, size, err := hashing.HashReader(&ctxReader{ctx: ctx, r: p.f})
	if err != nil {
		return Descriptor{}, fmt.Errorf("cas: hashing the assembled %s: %w", p.blob, err)
	}
	if err := p.f.Close(); err != nil {
		return Descriptor{}, fmt.Errorf("cas: closing the staging file for %s: %w", p.blob, err)
	}
	p.closed = true
	defer p.store.inflight.Delete(p.blob.String())

	if !got.Equal(expected) {
		// Quarantined rather than deleted, on the same reasoning PutExpecting
		// applies: bytes that were offered as this blob and are not are
		// evidence — of a source serving the wrong thing, of an index that
		// pointed somewhere stale, or of an assembly bug here (ADR-0018).
		corrupt := &Corruption{Hash: expected, Actual: got, Size: size}
		dst, qErr := p.store.quarantineFile(p.path, expected)
		if qErr != nil {
			return Descriptor{}, errors.Join(corrupt, qErr)
		}
		corrupt.Path = dst
		return Descriptor{}, corrupt
	}

	deduped, err := p.store.publish(p.path, expected)
	if err != nil {
		return Descriptor{}, err
	}
	// The staging name is linked, not moved, so it is still there. Removing it
	// is the tidy-up; the blob is already addressable.
	_ = os.Remove(p.path)
	return Descriptor{Hash: expected, Size: size, Materialised: Copy, Deduplicated: deduped}, nil
}

func (p *filePartial) Discard() error {
	if p.closed {
		return nil
	}
	p.closed = true
	defer p.store.inflight.Delete(p.blob.String())
	if err := p.f.Close(); err != nil {
		return fmt.Errorf("cas: closing the staging file for %s: %w", p.blob, err)
	}
	if err := os.Remove(p.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cas: discarding the staging file for %s: %w", p.blob, err)
	}
	return nil
}

func (p *filePartial) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	defer p.store.inflight.Delete(p.blob.String())
	if err := p.f.Sync(); err != nil {
		return fmt.Errorf("cas: syncing the staging file for %s: %w", p.blob, err)
	}
	if err := p.f.Close(); err != nil {
		return fmt.Errorf("cas: closing the staging file for %s: %w", p.blob, err)
	}
	return nil
}
