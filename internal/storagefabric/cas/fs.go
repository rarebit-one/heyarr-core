package cas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// LayoutVersion is the on-disk format version, recorded in the marker file so a
// future layout change can be detected rather than misread.
const LayoutVersion = 1

// MarkerName identifies a directory as a Heyarr CAS root.
const MarkerName = "HEYARR_CAS"

// Directory names within the root.
const (
	blobsDir      = "blobs"
	tmpDir        = "tmp"
	quarantineDir = "quarantine"
)

// fanout splits a digest into nested directories. Two levels of two hex
// characters gives at most 65 536 leaf directories — flat enough that `find`
// and a filesystem's directory index both stay usable, deep enough that no
// single directory holds a million entries.
const (
	fanoutLevels = 2
	fanoutWidth  = 2
)

// Permissions. Blobs are read-only by convention: they are immutable, and a
// tool that opens one for writing should have to work at it.
const (
	dirPerm  fs.FileMode = 0o750
	blobPerm fs.FileMode = 0o440
	tempPerm fs.FileMode = 0o640
)

// Marker is the contents of the CAS root marker file.
type Marker struct {
	Version int    `json:"version"`
	Algo    string `json:"algo"`
	Fanout  []int  `json:"fanout"`
	PeerID  string `json:"peer_id,omitempty"`
}

// FS is a Store backed by a local directory tree.
type FS struct {
	root string
}

var _ Store = (*FS)(nil)

// OpenFS prepares a CAS rooted at root, creating and marking it if absent.
//
// If a marker already exists it is validated: a root written by a future layout
// must be refused rather than misread, on the same reasoning as the schema
// downgrade guard (ADR-0003) — an old binary interpreting a new layout does not
// fail loudly, it silently does the wrong thing.
func OpenFS(root string) (*FS, error) {
	if root == "" {
		return nil, errors.New("cas: root must be set")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("cas: resolving %s: %w", root, err)
	}
	for _, dir := range []string{abs, filepath.Join(abs, blobsDir), filepath.Join(abs, tmpDir), filepath.Join(abs, quarantineDir)} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, fmt.Errorf("cas: creating %s: %w", dir, err)
		}
	}
	s := &FS{root: abs}
	if err := s.ensureMarker(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FS) ensureMarker() error {
	_, err := s.readMarker()
	if errors.Is(err, fs.ErrNotExist) {
		return s.writeMarker(Marker{
			Version: LayoutVersion,
			Algo:    hashing.Algorithm,
			Fanout:  []int{fanoutWidth, fanoutWidth},
		})
	}
	return err
}

// MarkerPath is the file that identifies this directory as a CAS root.
//
// It is exported because the identity check reports it by name: an operator
// told two identities disagree needs to be told where the second one is
// written, not left to find it (ADR-0010).
func (s *FS) MarkerPath() string { return filepath.Join(s.root, MarkerName) }

// readMarker loads and validates the marker.
//
// Validation lives here rather than at each call site because a marker that is
// wrong is wrong for every reader: a root written by a future layout must be
// refused rather than misread, on the same reasoning as the schema downgrade
// guard (ADR-0003).
func (s *FS) readMarker() (Marker, error) {
	path := s.MarkerPath()
	data, err := os.ReadFile(path) // #nosec G304 -- path is derived from the configured root
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Marker{}, err
	case err != nil:
		return Marker{}, fmt.Errorf("cas: reading marker: %w", err)
	}

	var marker Marker
	if err := json.Unmarshal(data, &marker); err != nil {
		return Marker{}, fmt.Errorf("cas: %s is not a valid marker: %w", path, err)
	}
	if marker.Version > LayoutVersion {
		return Marker{}, fmt.Errorf("cas: %s was written with layout version %d, this binary understands %d — "+
			"upgrade rather than downgrade", s.root, marker.Version, LayoutVersion)
	}
	if marker.Algo != "" && marker.Algo != hashing.Algorithm {
		return Marker{}, fmt.Errorf("cas: %s uses hash algorithm %q, this binary uses %q",
			s.root, marker.Algo, hashing.Algorithm)
	}
	return marker, nil
}

// writeMarker replaces the marker atomically.
//
// Through a temporary file in the root and a rename, so that a process killed
// mid-write leaves the previous marker rather than a truncated one. A CAS root
// whose marker is half-written is a root nothing can identify, and the identity
// check would read that as a disagreement.
func (s *FS) writeMarker(marker Marker) error {
	encoded, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("cas: encoding marker: %w", err)
	}
	tmp, err := os.CreateTemp(s.root, ".marker-*.tmp")
	if err != nil {
		return fmt.Errorf("cas: writing marker: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(append(encoded, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cas: writing marker: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cas: writing marker: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cas: writing marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cas: writing marker: %w", err)
	}
	if err := os.Rename(name, s.MarkerPath()); err != nil {
		return fmt.Errorf("cas: writing marker: %w", err)
	}
	return syncDir(s.root)
}

// MarkerPeerID reports the peer this CAS root is bound to, or "" if it is not
// bound yet.
//
// This is one half of the two-places peer identity (ADR-0010); the database is
// the other. The store deliberately does NOT compare them — it cannot see the
// database, and a comparison implemented in two packages is a comparison that
// gets removed from one of them.
func (s *FS) MarkerPeerID() (string, error) {
	marker, err := s.readMarker()
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return marker.PeerID, nil
}

// BindPeer records the peer that owns this CAS root.
//
// It writes what it is given, unconditionally. Deciding whether writing is the
// right thing to do requires knowing what the database says, which is the
// caller's job — see internal/peer/identity.
func (s *FS) BindPeer(peerID string) error {
	if peerID == "" {
		return errors.New("cas: refusing to bind the root to an empty peer id")
	}
	marker, err := s.readMarker()
	if errors.Is(err, fs.ErrNotExist) {
		marker = Marker{Version: LayoutVersion, Algo: hashing.Algorithm, Fanout: []int{fanoutWidth, fanoutWidth}}
	} else if err != nil {
		return err
	}
	marker.PeerID = peerID
	return s.writeMarker(marker)
}

// Root is the directory this store occupies.
func (s *FS) Root() string { return s.root }

// LocalPath returns the file backing a blob, checking it is there first: a
// path to something absent is worse than an error, because the caller finds
// out from a subprocess that failed for reasons it cannot explain.
func (s *FS) LocalPath(ctx context.Context, h hashing.Hash) (string, error) {
	if _, err := s.Stat(ctx, h); err != nil {
		return "", err
	}
	return s.blobPath(h), nil
}

// blobPath is the canonical location of a blob's bytes.
func (s *FS) blobPath(h hashing.Hash) string {
	hex := h.Hex()
	parts := make([]string, 0, fanoutLevels+3)
	parts = append(parts, s.root, blobsDir, hashing.Algorithm)
	for i := range fanoutLevels {
		parts = append(parts, hex[i*fanoutWidth:(i+1)*fanoutWidth])
	}
	return filepath.Join(append(parts, hex)...)
}

// Put streams r into the store, hashing as it writes.
//
// The bytes go to a temporary file in the same directory tree, so the final
// rename is atomic on the same filesystem. A process killed mid-Put therefore
// leaves nothing addressable — only a reapable file under tmp/.
func (s *FS) Put(ctx context.Context, r io.Reader) (Descriptor, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.root, tmpDir), "put-*.part")
	if err != nil {
		return Descriptor{}, fmt.Errorf("cas: creating temporary file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: on the success path the file has already been
	// renamed away and these are no-ops.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(tempPerm); err != nil {
		return Descriptor{}, fmt.Errorf("cas: setting temporary file mode: %w", err)
	}

	hasher := hashing.New()
	buf := make([]byte, 1<<20)
	written, err := io.CopyBuffer(io.MultiWriter(tmp, hasher), &ctxReader{ctx: ctx, r: r}, buf)
	if err != nil {
		return Descriptor{}, fmt.Errorf("cas: writing blob: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return Descriptor{}, fmt.Errorf("cas: syncing blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Descriptor{}, fmt.Errorf("cas: closing blob: %w", err)
	}

	h := hasher.Sum()
	desc := Descriptor{Hash: h, Size: written, Materialised: Copy}
	deduped, err := s.commit(tmpName, h)
	if err != nil {
		return Descriptor{}, err
	}
	desc.Deduplicated = deduped
	return desc, nil
}

// commit moves a finished temporary file into its final location. It reports
// whether the blob was already present, in which case the temporary file is
// discarded — deduplication is a consequence of the layout, not a feature.
func (s *FS) commit(tmpName string, h hashing.Hash) (deduplicated bool, err error) {
	final := s.blobPath(h)
	if err := os.MkdirAll(filepath.Dir(final), dirPerm); err != nil {
		return false, fmt.Errorf("cas: creating blob directory: %w", err)
	}
	if _, err := os.Stat(final); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("cas: checking for an existing blob: %w", err)
	}

	if err := os.Chmod(tmpName, blobPerm); err != nil {
		return false, fmt.Errorf("cas: setting blob mode: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return false, fmt.Errorf("cas: publishing blob: %w", err)
	}
	// fsync the parent directory too. Without it the rename can be lost by a
	// crash even though the file's own contents were synced, which would leave
	// the catalog referencing a blob the filesystem never recorded.
	if err := syncDir(filepath.Dir(final)); err != nil {
		return false, err
	}
	return false, nil
}

// Link materialises srcPath into the store, degrading down the ladder when the
// filesystem cannot oblige (ADR-0014).
//
// The source is hashed first, because the destination path is derived from the
// content. That means one full read for a reflink or hardlink, and for a copy
// the hash is computed during the copy rather than in a separate pass.
func (s *FS) Link(ctx context.Context, srcPath string, mode Materialisation) (Descriptor, error) {
	h, size, err := hashing.HashFile(srcPath)
	if err != nil {
		return Descriptor{}, err
	}
	final := s.blobPath(h)
	if _, err := os.Stat(final); err == nil {
		return Descriptor{Hash: h, Size: size, Materialised: mode, Deduplicated: true}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Descriptor{}, fmt.Errorf("cas: checking for an existing blob: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(final), dirPerm); err != nil {
		return Descriptor{}, fmt.Errorf("cas: creating blob directory: %w", err)
	}

	for _, attempt := range ladder(mode) {
		used, err := s.materialise(ctx, srcPath, final, attempt)
		if err == nil {
			if err := syncDir(filepath.Dir(final)); err != nil {
				return Descriptor{}, err
			}
			return Descriptor{Hash: h, Size: size, Materialised: used}, nil
		}
		if !errors.Is(err, errDegrade) {
			return Descriptor{}, err
		}
		// Fall through to the next rung. A cross-filesystem source, a
		// filesystem without cloning, or a hardlink limit are all ordinary and
		// must degrade rather than fail (ADR-0014).
	}
	return Descriptor{}, fmt.Errorf("cas: could not materialise %s into the store", srcPath)
}

// errDegrade signals that this rung of the ladder is not available here.
var errDegrade = errors.New("cas: materialisation unavailable, degrading")

func ladder(mode Materialisation) []Materialisation {
	switch mode {
	case Reflink:
		return []Materialisation{Reflink, Hardlink, Copy}
	case Hardlink:
		return []Materialisation{Hardlink, Copy}
	default:
		return []Materialisation{Copy}
	}
}

func (s *FS) materialise(ctx context.Context, src, dst string, mode Materialisation) (Materialisation, error) {
	switch mode {
	case Reflink:
		if err := reflink(src, dst); err != nil {
			return "", err
		}
		return Reflink, nil
	case Hardlink:
		if err := os.Link(src, dst); err != nil {
			return "", fmt.Errorf("%w: %w", errDegrade, err)
		}
		if err := os.Chmod(dst, blobPerm); err != nil {
			// A hardlink shares the inode, so the mode change also affects the
			// source. Not fatal, and not worth failing an ingest over.
			_ = err
		}
		return Hardlink, nil
	default:
		if err := s.copyFile(ctx, src, dst); err != nil {
			return "", err
		}
		return Copy, nil
	}
}

func (s *FS) copyFile(ctx context.Context, src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- src comes from a configured library root
	if err != nil {
		return fmt.Errorf("cas: opening %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Join(s.root, tmpDir), "link-*.part")
	if err != nil {
		return fmt.Errorf("cas: creating temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	buf := make([]byte, 1<<20)
	if _, err := io.CopyBuffer(tmp, &ctxReader{ctx: ctx, r: in}, buf); err != nil {
		return fmt.Errorf("cas: copying %s: %w", src, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("cas: syncing copy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cas: closing copy: %w", err)
	}
	if err := os.Chmod(tmpName, blobPerm); err != nil {
		return fmt.Errorf("cas: setting blob mode: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("cas: publishing copy: %w", err)
	}
	return nil
}

// Open returns a seekable reader over a blob.
func (s *FS) Open(_ context.Context, h hashing.Hash) (ReadSeekCloser, Descriptor, error) {
	path := s.blobPath(h)
	f, err := os.Open(path) // #nosec G304 -- path is derived from a validated hash
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, Descriptor{}, fmt.Errorf("%w: %s", ErrNotFound, h)
		}
		return nil, Descriptor{}, fmt.Errorf("cas: opening %s: %w", h, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Descriptor{}, fmt.Errorf("cas: stat %s: %w", h, err)
	}
	return f, Descriptor{Hash: h, Size: info.Size()}, nil
}

// Stat reports what is known about a blob without reading it.
func (s *FS) Stat(_ context.Context, h hashing.Hash) (Descriptor, error) {
	info, err := os.Stat(s.blobPath(h))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Descriptor{}, fmt.Errorf("%w: %s", ErrNotFound, h)
		}
		return Descriptor{}, fmt.Errorf("cas: stat %s: %w", h, err)
	}
	return Descriptor{Hash: h, Size: info.Size(), ModTime: info.ModTime()}, nil
}

// Has reports presence without verifying contents.
func (s *FS) Has(_ context.Context, h hashing.Hash) (bool, error) {
	_, err := os.Stat(s.blobPath(h))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("cas: stat %s: %w", h, err)
	}
}

// Verify re-reads a blob and confirms it still hashes to its own name. On
// mismatch the blob is moved to quarantine rather than deleted: it may be the
// only copy, and on a hardlink-ingested library it may be the original that
// changed (ADR-0018).
func (s *FS) Verify(ctx context.Context, h hashing.Hash) error {
	rc, _, err := s.Open(ctx, h)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	if _, err := hashing.Verify(&ctxReader{ctx: ctx, r: rc}, h); err != nil {
		var mismatch *hashing.ErrMismatch
		if errors.As(err, &mismatch) {
			corrupt := &Corruption{Hash: h, Actual: mismatch.Got, Size: mismatch.Size}
			dst, qErr := s.quarantine(h)
			if qErr != nil {
				return errors.Join(corrupt, qErr)
			}
			corrupt.Path = dst
			return corrupt
		}
		return err
	}
	return nil
}

// quarantine moves a blob out of the addressable tree, preserving it for
// inspection, and reports where it went.
//
// Moving rather than deleting is ADR-0018 and it is load-bearing rather than
// cautious: a blob materialised as a hardlink shares its inode with the file
// it was adopted from, so "these bytes changed" frequently means an external tool rewrote the
// operator's original — and on hyperion-1 hardlink is the outcome for every
// file (#43). Deleting would destroy the only copy of something that was never
// Heyarr's to delete.
func (s *FS) quarantine(h hashing.Hash) (string, error) {
	dst := filepath.Join(s.root, quarantineDir,
		fmt.Sprintf("%s.%d", h.Hex(), time.Now().UTC().UnixNano()))
	if err := os.Chmod(s.blobPath(h), tempPerm); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("cas: preparing to quarantine %s: %w", h, err)
	}
	if err := os.Rename(s.blobPath(h), dst); err != nil {
		return "", fmt.Errorf("cas: quarantining %s: %w", h, err)
	}
	return dst, nil
}

// Delete removes a blob. The store does not know about references; establishing
// that nothing points at it is the caller's job (ADR-0018).
func (s *FS) Delete(_ context.Context, h hashing.Hash) error {
	path := s.blobPath(h)
	if err := os.Chmod(path, tempPerm); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cas: preparing to delete %s: %w", h, err)
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, h.String())
		}
		return fmt.Errorf("cas: deleting %s: %w", h, err)
	}
	return nil
}

// Walk visits every blob in the store.
//
// A file whose name is not a valid hash is skipped rather than reported: the
// tree may contain a partially written rename or something an operator left
// behind, and neither should stop an integrity sweep.
func (s *FS) Walk(ctx context.Context, fn func(Descriptor) error) error {
	base := filepath.Join(s.root, blobsDir, hashing.Algorithm)
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		h, parseErr := hashing.Parse(hashing.Algorithm + ":" + d.Name())
		if parseErr != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(Descriptor{Hash: h, Size: info.Size(), ModTime: info.ModTime()})
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cas: walking %s: %w", base, err)
	}
	return nil
}

// TempFile is a partial write left behind by an interrupted Put or Link.
type TempFile struct {
	// Name is the base name within tmp/, never a path. Callers pass it back to
	// RemoveTemp, which is what keeps a caller from asking the store to unlink
	// something outside its own tree.
	Name    string
	Size    int64
	ModTime time.Time
}

// TempFiles lists the partial writes currently on disk.
//
// Listing and removing are separate operations because garbage collection is
// dry-run by default (ADR-0018): "here is what I would delete" has to be
// answerable without deleting anything.
func (s *FS) TempFiles() ([]TempFile, error) {
	dir := filepath.Join(s.root, tmpDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("cas: reading %s: %w", dir, err)
	}
	var out []TempFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// The file went away between the listing and the stat, which is
			// exactly what a concurrent reap looks like. Not a finding.
			continue
		}
		out = append(out, TempFile{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	return out, nil
}

// RemoveTemp deletes one partial write by the name TempFiles reported.
//
// It refuses anything that is not a bare .part base name. The store owns its
// layout and nothing outside this package may assume it (§18); accepting a path
// here would make "delete this file" an operation the caller aims, and garbage
// collection is the one caller you least want holding a loaded one.
func (s *FS) RemoveTemp(name string) error {
	if name == "" || name != filepath.Base(name) || !strings.HasSuffix(name, ".part") {
		return fmt.Errorf("cas: %q is not a temporary file name", name)
	}
	if err := os.Remove(filepath.Join(s.root, tmpDir, name)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cas: removing temporary file %s: %w", name, err)
	}
	return nil
}

// ReapTemp removes temporary files left behind by interrupted writes that are
// older than age. An interrupted Put leaves nothing addressable, but it does
// leave bytes on disk, and nothing else will clean them up.
func (s *FS) ReapTemp(olderThan time.Duration) (int, error) {
	files, err := s.TempFiles()
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	var removed int
	for _, f := range files {
		if f.ModTime.After(cutoff) {
			continue
		}
		if err := s.RemoveTemp(f.Name); err == nil {
			removed++
		}
	}
	return removed, nil
}

// syncDir fsyncs a directory so a rename within it survives a crash.
func syncDir(path string) error {
	d, err := os.Open(path) // #nosec G304 -- path is within the configured root
	if err != nil {
		return fmt.Errorf("cas: opening %s to sync: %w", path, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("cas: syncing %s: %w", path, err)
	}
	return nil
}

// ctxReader makes a long read cancellable. Hashing a 60 GB file should stop
// when the job's lease is lost (ADR-0008), not run to completion first.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Quarantined is a blob the store moved out of the addressable tree because
// its bytes stopped matching their own name (ADR-0018).
type Quarantined struct {
	// Hash is the name the blob was stored under, which is the digest its
	// bytes were expected to have. Identity is still the hash, even here
	// (ADR-0005) — quarantine changes where the bytes are, not what they were
	// claiming to be.
	Hash hashing.Hash
	// Name is the base name within quarantine/, never a path.
	Name string
	Size int64
	// QuarantinedAt is when the store moved the bytes aside, taken from the
	// nanosecond suffix quarantine() writes into the filename rather than from
	// the file's mtime — a rename preserves mtime, so mtime is when the bytes
	// were last WRITTEN, which for a blob corrupted in place is the moment
	// somebody else damaged it.
	QuarantinedAt time.Time
}

// QuarantinedBlobs lists what this store has quarantined.
//
// It is on *FS rather than on Store, like TempFiles above: it is a question
// about local on-disk layout, and a store with no local layout has no honest
// answer to it. The caller that needs it — a peer building the inventory it
// reports to the controller (M4-07) — is a caller that already knows it is
// looking at a local store.
//
// It exists because of what an inventory that could not answer it would say.
// A quarantined blob is neither present nor absent: the peer HAS the bytes and
// they are NOT servable. Omitting it would report the blob as gone and invite
// a replacement transfer that overwrites the evidence; reporting it as present
// would leave the controller believing in a copy that cannot be read. Both are
// worse than the truth, which is `corrupt`.
//
// A blob quarantined more than once — corrupted, replaced, corrupted again —
// has several files here. The most recent one wins, because that is the state
// of the blob now.
func (s *FS) QuarantinedBlobs() ([]Quarantined, error) {
	dir := filepath.Join(s.root, quarantineDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("cas: reading %s: %w", dir, err)
	}
	latest := map[hashing.Hash]Quarantined{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		hexPart, nanosPart, ok := strings.Cut(e.Name(), ".")
		if !ok {
			continue
		}
		h, parseErr := hashing.Parse(hashing.Algorithm + ":" + hexPart)
		if parseErr != nil {
			continue
		}
		nanos, convErr := strconv.ParseInt(nanosPart, 10, 64)
		if convErr != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// Gone between the listing and the stat. Not a finding.
			continue
		}
		q := Quarantined{
			Hash:          h,
			Name:          e.Name(),
			Size:          info.Size(),
			QuarantinedAt: time.Unix(0, nanos).UTC(),
		}
		if prev, seen := latest[h]; seen && prev.QuarantinedAt.After(q.QuarantinedAt) {
			continue
		}
		latest[h] = q
	}
	out := make([]Quarantined, 0, len(latest))
	for _, q := range latest {
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash.String() < out[j].Hash.String() })
	return out, nil
}
