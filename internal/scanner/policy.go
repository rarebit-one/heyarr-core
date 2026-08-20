package scanner

import (
	"path"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
)

// Policy decides which of the files under a root are candidates for ingest,
// and what media type each one carries.
//
// The default policy is deliberately conservative: a media library is full of
// things that are not content — a downloader's partial files, a NAS's thumbnail
// database, an editor's swap file — and enqueueing an ingest for each of them
// means hashing gigabytes of rubbish and then carrying it in the catalog
// forever. Skipping is cheap and reversible; ingesting is neither.
type Policy struct {
	// Include, when non-empty, restricts candidates to paths matching at least
	// one pattern. Patterns are path.Match globs, tried against the
	// slash-separated path relative to the root and against the base name, so
	// "*.mkv" works at any depth.
	Include []string
	// Exclude removes candidates, and beats Include. Its patterns match the
	// same two ways.
	Exclude []string
	// Extensions maps a lowercased extension (including the dot) to a media
	// type. When it is non-empty it is the whole of the extension policy: an
	// extension not named here is not a candidate. When it is empty the
	// built-in table (ingest.MIMEForExtension) decides, and an extension it
	// does not know is skipped.
	Extensions map[string]string
	// MaxDepth bounds directory recursion. Zero means DefaultMaxDepth. It is a
	// backstop against a symlink loop on a platform where the inode-based loop
	// detector cannot run (see fingerprint_windows.go), not the primary
	// defence.
	MaxDepth int
}

// DefaultMaxDepth is deep enough for any real library layout and shallow enough
// that a pathological one terminates.
const DefaultMaxDepth = 64

// noiseNames are files and directories that are never content, whatever their
// extension. @eaDir is Synology's per-directory metadata store and holds a
// full-size copy of many files, which is a very expensive thing to ingest by
// accident.
var noiseNames = map[string]bool{
	".DS_Store":    true,
	"Thumbs.db":    true,
	"desktop.ini":  true,
	"@eaDir":       true,
	".@__thumb":    true,
	"lost+found":   true,
	".Trashes":     true,
	"$RECYCLE.BIN": true,
}

// noiseExtensions mark a file that is still being written or is a working copy.
// Ingesting a partial download produces a blob whose hash describes bytes that
// never existed as a complete file, and nothing later can tell that from a
// corrupt copy.
var noiseExtensions = map[string]bool{
	".part":       true, // wget, Firefox, qBittorrent
	".!qb":        true, // qBittorrent (".!qB" on disk)
	".!ut":        true, // uTorrent
	".crdownload": true, // Chrome
	".partial":    true,
	".tmp":        true,
	".temp":       true,
	".swp":        true,
	".bak":        true,
	".filepart":   true,
}

// Skip reasons, recorded on the debug log line so that "why was my file not
// picked up" is answerable without reading this file.
const (
	reasonHidden      = "hidden"
	reasonNoise       = "noise"
	reasonExcluded    = "excluded"
	reasonNotIncluded = "not-included"
	reasonExtension   = "unknown-extension"
)

// AllowDir reports whether the scanner should descend into a directory, and why
// not when it should not.
func (p Policy) AllowDir(name string) (bool, string) {
	switch {
	case isHidden(name):
		return false, reasonHidden
	case noiseNames[name]:
		return false, reasonNoise
	}
	return true, ""
}

// AllowFile reports whether a file is a candidate for ingest and, if so, the
// media type to record for it.
//
// relPath is slash-separated and relative to the library root.
func (p Policy) AllowFile(relPath string) (mime string, ok bool, reason string) {
	name := path.Base(relPath)
	switch {
	case isHidden(name):
		return "", false, reasonHidden
	case noiseNames[name]:
		return "", false, reasonNoise
	}

	ext := ingest.Ext(name)
	if noiseExtensions[ext] {
		return "", false, reasonNoise
	}
	if matchAny(p.Exclude, relPath, name) {
		return "", false, reasonExcluded
	}
	if len(p.Include) > 0 && !matchAny(p.Include, relPath, name) {
		return "", false, reasonNotIncluded
	}

	if len(p.Extensions) > 0 {
		mime, known := p.Extensions[ext]
		if !known {
			return "", false, reasonExtension
		}
		return mime, true, ""
	}
	// An unknown extension is skipped rather than ingested with no media type.
	// The catalog would accept it, but a library full of unidentifiable rows is
	// how the *arr scanners earn their reputation, and adding an extension to
	// the policy is a one-line change an operator can make.
	if mime := ingest.MIMEForExtension(ext); mime != "" {
		return mime, true, ""
	}
	return "", false, reasonExtension
}

// isHidden reports a dotfile. Hidden files and directories are skipped as a
// class: they hold configuration, version-control metadata and application
// state, and a library that keeps content in one is broken in a way no scanner
// should paper over.
func isHidden(name string) bool {
	return len(name) > 1 && name[0] == '.'
}

// matchAny tries each pattern against the whole relative path and against the
// base name. Matching both is what lets "*.nfo" mean "any .nfo anywhere"
// without path.Match growing a ** it does not have — and without this package
// taking a dependency on a glob library to say something this small.
func matchAny(patterns []string, relPath, name string) bool {
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		if ok, err := path.Match(pattern, relPath); err == nil && ok {
			return true
		}
		if ok, err := path.Match(pattern, name); err == nil && ok {
			return true
		}
	}
	return false
}

// normaliseExtensions lowercases the keys of a configured extension table, so
// that ".MKV" in a config file behaves the same as ".mkv".
func normaliseExtensions(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for ext, mime := range in {
		out[strings.ToLower(ext)] = mime
	}
	return out
}
