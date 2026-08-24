package cas

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// ErrNotVerified is a staged file offered for publication under a name its
// bytes do not have.
//
// It is the last gate rather than the only one — the caller is expected to
// have compared the digests itself and stopped — but it is the gate that
// cannot be reordered away, and Invariant 1 is worth being told twice.
var ErrNotVerified = errors.New("cas: staged bytes do not hash to the name they were offered under")

// Staged is a file being assembled inside the store's private staging area.
//
// It exists for ADR-0036: integrity repair reconstructs a whole replacement
// blob — intact local chunks plus replacements fetched from a peer — and needs
// somewhere to build it that is NEVER addressable. That is the same place, and
// the same publish, that Put, PutExpecting and Link already use; this type
// exposes the two halves separately so a caller can do something between them.
// Repair's something is quarantining the damaged original (ADR-0018), and that
// step has to sit between verification and publication.
//
// It is deliberately not a mutation primitive. There is no seek, no offset, no
// way to reach an addressable file: bytes go in in order, the digest comes out,
// and the only way to make them addressable is Publish under the digest they
// actually have.
type Staged struct {
	fs   *FS
	f    *os.File
	path string

	hasher  *hashing.Hasher
	written int64

	closed    bool
	digest    hashing.Hash
	published bool
}

// Stage opens a private staging file, hashing whatever is written to it.
//
// The file lives under tmp/ with the .part suffix, so a process that dies
// holding one leaves something ReapTemp will clean up rather than a stray file
// in the blob tree — and, critically, nothing that answers to any digest.
func (s *FS) Stage() (*Staged, error) {
	if err := os.MkdirAll(filepath.Join(s.root, tmpDir), dirPerm); err != nil {
		return nil, fmt.Errorf("cas: creating the staging directory: %w", err)
	}
	path := s.stagingPathWith("stage")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, tempPerm) // #nosec G304 -- the name is generated within the configured root
	if err != nil {
		return nil, fmt.Errorf("cas: creating a staging file: %w", err)
	}
	return &Staged{fs: s, f: f, path: path, hasher: hashing.New()}, nil
}

// Write appends bytes to the staging file and to the running digest.
func (t *Staged) Write(p []byte) (int, error) {
	if t.closed {
		return 0, errors.New("cas: writing to a staging file that has already been finished")
	}
	n, err := t.f.Write(p)
	t.written += int64(n)
	// Hash exactly what reached the file, so a short write cannot leave the
	// digest describing bytes that are not there.
	if _, hErr := t.hasher.Write(p[:n]); hErr != nil && err == nil {
		err = hErr
	}
	if err != nil {
		return n, fmt.Errorf("cas: writing staged bytes: %w", err)
	}
	return n, nil
}

// Written is how many bytes have been staged.
func (t *Staged) Written() int64 { return t.written }

// Digest finishes the file and returns the whole-object digest of everything
// written to it (ADR-0005).
//
// It syncs and closes, so the bytes the digest describes are the bytes on
// disk. Calling it twice returns the same answer rather than failing: a caller
// that verifies and then publishes should not have to remember which call
// closed the file.
func (t *Staged) Digest() (hashing.Hash, error) {
	if t.closed {
		return t.digest, nil
	}
	if err := t.f.Sync(); err != nil {
		return hashing.Hash{}, fmt.Errorf("cas: syncing staged bytes: %w", err)
	}
	if err := t.f.Close(); err != nil {
		return hashing.Hash{}, fmt.Errorf("cas: closing staged bytes: %w", err)
	}
	t.closed = true
	t.digest = t.hasher.Sum()
	return t.digest, nil
}

// Publish makes the staged bytes addressable as h, by the same atomic link
// every other write to the store uses.
//
// It refuses unless the bytes actually hash to h. Publishing is the one
// operation in this package that creates an addressable name, and a name that
// does not describe its contents is the single thing content addressing exists
// to make impossible (Invariant 1).
func (t *Staged) Publish(h hashing.Hash) (Descriptor, error) {
	if t.published {
		return Descriptor{}, errors.New("cas: staged bytes have already been published")
	}
	got, err := t.Digest()
	if err != nil {
		return Descriptor{}, err
	}
	if !got.Equal(h) {
		return Descriptor{}, fmt.Errorf("%w: offered as %s, hashes to %s", ErrNotVerified, h, got)
	}
	deduped, err := t.fs.publish(t.path, h)
	if err != nil {
		return Descriptor{}, err
	}
	t.published = true
	// publish links rather than renames, so the staging name still exists and
	// still refers to the published inode. Removing it is the tidy-up, exactly
	// as in Put.
	_ = os.Remove(t.path)
	return Descriptor{Hash: h, Size: t.written, Materialised: Copy, Deduplicated: deduped}, nil
}

// Discard throws the staged bytes away. Safe to call after Publish, where it
// is a no-op, so a caller can defer it unconditionally.
func (t *Staged) Discard() error {
	if !t.closed {
		_ = t.f.Close()
		t.closed = true
	}
	if t.published {
		return nil
	}
	if err := os.Remove(t.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cas: discarding staged bytes: %w", err)
	}
	return nil
}

// Quarantine moves a blob out of the addressable tree and reports where it
// went, without first reading it.
//
// Verify quarantines what it has just proved corrupt. This is for the caller
// that already knows — integrity repair, which located the damage chunk by
// chunk against a manifest and is about to publish a replacement. The damaged
// bytes are still evidence and are still never deleted (ADR-0018).
func (s *FS) Quarantine(h hashing.Hash) (string, error) {
	if _, err := os.Stat(s.blobPath(h)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, h.String())
		}
		return "", fmt.Errorf("cas: stat %s: %w", h, err)
	}
	return s.quarantine(h)
}

// OpenQuarantined opens a quarantined artefact by the base name
// QuarantinedBlobs reported.
//
// Quarantined bytes are evidence, and they are also the best available source
// of the parts of a damaged blob that are still intact — a repair driven by a
// finding the checker already quarantined has nowhere else to read them from.
// Read-only, and it refuses anything that is not a bare name in quarantine/,
// for the reason RemoveTemp refuses one: the store owns its layout, and a
// caller that can aim this at a path can aim it outside the root (§18).
func (s *FS) OpenQuarantined(name string) (ReadSeekCloser, error) {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("cas: %q is not a quarantined artefact name", name)
	}
	f, err := os.Open(filepath.Join(s.root, quarantineDir, name)) // #nosec G304 -- a base name within the configured root
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: quarantine/%s", ErrNotFound, name)
		}
		return nil, fmt.Errorf("cas: opening quarantine/%s: %w", name, err)
	}
	return f, nil
}
