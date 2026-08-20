package scanner

import (
	"io"
	"os"
)

// FS is the filesystem the scanner walks.
//
// It is an interface for one reason, and it is the reason the whole package
// exists: the value of the fingerprint cache is that an unchanged file is never
// READ, and "we did not read it" can only be asserted by counting opens. Timing
// a scan proves nothing — a warm page cache makes a broken cache look fast.
//
// The methods take absolute host paths rather than io/fs's rooted, slashed
// names, because a library root is an absolute path on a specific machine and
// re-rooting it here would only mean un-rooting it again before every stat.
type FS interface {
	// ReadDir lists a directory. Entries are not required to be sorted; the
	// scanner sorts them, so that a scan is deterministic (ADR-0017).
	ReadDir(name string) ([]os.DirEntry, error)
	// Lstat describes a path without following a final symlink.
	Lstat(name string) (os.FileInfo, error)
	// Stat describes a path, following symlinks. A dangling symlink fails here
	// and is skipped rather than fatal.
	Stat(name string) (os.FileInfo, error)
	// Open opens a file for reading. The scanner calls it ONLY for a file it
	// has already decided to enqueue — see Scanner.readable — so an unchanged
	// file is never opened, which is the property the tests count.
	Open(name string) (io.ReadCloser, error)
}

// OSFS is the real filesystem.
type OSFS struct{}

// ReadDir lists a directory.
func (OSFS) ReadDir(name string) ([]os.DirEntry, error) { return os.ReadDir(name) }

// Lstat describes a path without following a final symlink.
func (OSFS) Lstat(name string) (os.FileInfo, error) { return os.Lstat(name) }

// Stat describes a path, following symlinks.
func (OSFS) Stat(name string) (os.FileInfo, error) { return os.Stat(name) }

// Open opens a file for reading.
func (OSFS) Open(name string) (io.ReadCloser, error) { return os.Open(name) } // #nosec G304 -- the path comes from walking a configured library root

var _ FS = OSFS{}
