// Package cas is the content-addressed blob store. It owns bytes and on-disk
// layout; nothing outside this package may assume either (spec §17, §18).
//
// The boundary is enforced by depguard, not convention: the content domain
// cannot import this package, and this package cannot import the domain
// (ADR-0006, ADR-0007). Crossing either line means an interface is missing.
package cas

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// Materialisation is how a source file becomes a blob in the store.
//
// The ladder exists because §60 keeps the *arr ecosystem's hardlink- and
// reflink-friendly workflows while §66 brings bytes under management, and those
// pull in opposite directions if ingest means "copy". On a filesystem with
// block cloning — ZFS 2.2, XFS, btrfs — adopting a 60 GB remux costs metadata
// only, which is the difference between being adoptable against a real library
// and requiring the operator to double their storage first (ADR-0014).
type Materialisation string

const (
	// Reflink tries copy-on-write cloning, then hardlink, then a byte copy.
	Reflink Materialisation = "reflink"
	// Hardlink shares the inode with the source. Cheapest, and it means an
	// external tool writing in place would corrupt the blob — which is why
	// integrity scanning exists and why corrupt blobs are quarantined rather
	// than deleted (ADR-0018).
	Hardlink Materialisation = "hardlink"
	// Copy always duplicates the bytes.
	Copy Materialisation = "copy"
)

// Errors returned by a Store.
var (
	// ErrNotFound means the store holds no blob with that hash.
	ErrNotFound = errors.New("cas: blob not found")
	// ErrCorrupt means stored bytes did not hash to their own name. The blob is
	// quarantined rather than removed, because it may be the only copy and
	// because on a hardlink-ingested library it may be the *original* that was
	// modified.
	ErrCorrupt = errors.New("cas: stored bytes do not match their hash")
)

// Corruption is the detail behind ErrCorrupt: what was expected, what was
// found, and where the bytes were preserved.
//
// It is a typed error rather than a formatted string because the quarantine
// path is the actionable half. "Blob X is corrupt" tells an operator to go
// looking; "the bytes are at quarantine/<hash>.<nanos>" tells them where, and
// lets the catalog record it as evidence (ADR-0018). It unwraps to ErrCorrupt,
// so callers that only care whether the bytes were bad are unaffected.
type Corruption struct {
	// Hash is the name the blob was stored under, which is also the digest its
	// bytes were expected to have (ADR-0005).
	Hash hashing.Hash
	// Actual is what the bytes hash to now.
	Actual hashing.Hash
	// Size is how many bytes were read.
	Size int64
	// Path is where the bytes were moved. Empty only when quarantining itself
	// failed, in which case that error is joined onto this one.
	Path string
}

func (c *Corruption) Error() string {
	return fmt.Sprintf("%s: %s hashes to %s over %d bytes, quarantined at %s",
		ErrCorrupt.Error(), c.Hash, c.Actual, c.Size, c.Path)
}

// Unwrap keeps errors.Is(err, ErrCorrupt) true.
func (c *Corruption) Unwrap() error { return ErrCorrupt }

// Descriptor is what the store knows about a blob. Deliberately not a domain
// object: the content domain never sees one of these.
type Descriptor struct {
	Hash hashing.Hash
	Size int64
	// ModTime is the file's modification time, populated by Stat and Walk.
	//
	// It is not identity and never will be (ADR-0005). Garbage collection uses
	// it only as a crude age for bytes that have no catalog row: a file written
	// seconds ago is far more likely to be an ingest that has not committed yet
	// than an orphan worth reclaiming (ADR-0018).
	ModTime time.Time
	// Materialised records how the bytes arrived, so an operator can tell
	// whether a blob shares storage with a file outside the store.
	Materialised Materialisation
	// Deduplicated reports that the bytes were already present, so nothing was
	// written. Ingest surfaces this rather than silently reporting a new blob.
	Deduplicated bool
}

// ReadSeekCloser is a blob's byte stream. Seeking is required, not optional:
// §28 makes HTTP range serving a contract that playback, remote probing,
// replication and web-seeding all depend on (ADR-0013).
type ReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

// Store holds immutable, content-addressed byte sequences.
//
// Every method takes a context because a remote implementation is expected
// later; the local one honours cancellation on long operations.
type Store interface {
	// Put streams r into the store, hashing as it writes, and returns the
	// resulting descriptor. Interrupting it must leave nothing addressable.
	Put(ctx context.Context, r io.Reader) (Descriptor, error)

	// Link materialises the file at srcPath into the store using mode,
	// degrading down the ladder when the filesystem cannot oblige.
	Link(ctx context.Context, srcPath string, mode Materialisation) (Descriptor, error)

	// Open returns a seekable reader over a blob's bytes.
	Open(ctx context.Context, h hashing.Hash) (ReadSeekCloser, Descriptor, error)

	// LocalPath returns a filesystem path for a blob's bytes, for the callers
	// that genuinely need one rather than a reader.
	//
	// There is exactly one such caller and it is an external process: FFmpeg
	// needs a seekable file it can open itself, and handing it a pipe means a
	// remux of a trailing-index container cannot work. Everything inside
	// Heyarr uses Open.
	//
	// It is on the Store rather than being reachable only through *FS because
	// the alternative is a type assertion at the call site, which is a
	// dependency on the implementation wearing a disguise. Note that this does
	// NOT relax ADR-0006: paths are still not identity, the domain still may
	// not call this, and a store with no local paths is free to return an
	// error rather than inventing one.
	LocalPath(ctx context.Context, h hashing.Hash) (string, error)

	// Stat reports what the store knows about a blob without opening it.
	Stat(ctx context.Context, h hashing.Hash) (Descriptor, error)

	// Has reports whether the blob is present. It does not verify contents;
	// that is Verify's job and it costs a full read.
	Has(ctx context.Context, h hashing.Hash) (bool, error)

	// Verify re-reads a blob and confirms it still hashes to its own name.
	// On mismatch it quarantines the blob and returns ErrCorrupt.
	Verify(ctx context.Context, h hashing.Hash) error

	// Delete removes a blob. Callers must have established that nothing
	// references it; the store does not know about references (ADR-0018).
	Delete(ctx context.Context, h hashing.Hash) error

	// Walk visits every blob in the store. The order is unspecified.
	Walk(ctx context.Context, fn func(Descriptor) error) error
}
