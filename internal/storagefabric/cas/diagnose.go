package cas

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// ErrStoreUnwritable is a store that cannot be written to at all, as opposed to
// one write that failed.
//
// It exists because the two deserve different answers. A single blob that could
// not be written is a job that failed and should be retried; a store whose root
// or whose whole blob tree refuses writes will refuse the next job too, and the
// one after that, and retrying is only a way of turning one fault into as many
// unrelated-looking failures as there are jobs (#151, where twelve ingests each
// reported the same store-wide fault as twelve separate job failures).
var ErrStoreUnwritable = errors.New("cas: the store cannot be written to")

// probeSeq keeps probe names unique within a process.
var probeSeq atomic.Uint64

// PermissionFault is a store write refused by the filesystem, carrying the
// evidence needed to say WHY without waiting for the fault to happen again.
//
// #151 was reported six times over four months and closed on a wrong diagnosis
// because every sighting was a single line of error text: a path and
// "permission denied". Nothing in it said whether the shard directory was the
// problem or its parent, what mode anything had, or whether the next write
// would have failed too. This type is that missing evidence, gathered at the
// moment of the failure and carried on the error, so the next occurrence is
// diagnosable from the log it already writes.
type PermissionFault struct {
	// Op is what the store was doing, in the words the error already used.
	Op string
	// Path is what the filesystem refused.
	Path string
	// Err is the refusal itself.
	Err error
	// Scope is how much of the store is affected: see Diagnosis.
	Scope Scope
	// Diagnosis is the rendered evidence.
	Diagnosis string
}

// Scope is how much of the store a permission fault covers.
type Scope string

const (
	// ScopePath is a fault confined to the path that failed — one poisoned
	// shard directory, say, while the rest of the store is fine.
	ScopePath Scope = "confined to the path that failed"
	// ScopeBlobTree is a fault covering the whole blob tree — every shard,
	// present and future.
	ScopeBlobTree Scope = "the whole blob tree"
	// ScopeStore is a fault covering the store root itself.
	ScopeStore Scope = "the whole store"
	// ScopeUnknown is a fault whose extent could not be established, because
	// the probes that would have established it failed for another reason.
	ScopeUnknown Scope = "unknown"
)

func (e *PermissionFault) Error() string {
	return fmt.Sprintf("cas: %s: %v\n%s", e.Op, e.Err, e.Diagnosis)
}

func (e *PermissionFault) Unwrap() error { return e.Err }

// Is reports a store-wide fault as ErrStoreUnwritable, so a caller can tell
// "this write failed" from "writing to this store is not going to work" without
// knowing anything about shards.
func (e *PermissionFault) Is(target error) bool {
	return target == ErrStoreUnwritable && (e.Scope == ScopeBlobTree || e.Scope == ScopeStore)
}

// StoreWide reports whether the fault covers more than the shard being written.
func (e *PermissionFault) StoreWide() bool {
	return e.Scope == ScopeBlobTree || e.Scope == ScopeStore
}

// permissionFault wraps a failed store write, attaching a diagnosis when the
// filesystem refused it on permissions.
//
// Anything else is wrapped the way it always was: a diagnosis of a disk that is
// full or a path that is not a directory would be noise, and the error already
// says which.
func (s *FS) permissionFault(op, path string, cause error) error {
	if !errors.Is(cause, fs.ErrPermission) {
		return fmt.Errorf("cas: %s: %w", op, cause)
	}
	fault := &PermissionFault{Op: op, Path: path, Err: cause}
	fault.Scope, fault.Diagnosis = s.diagnose(path)
	return fault
}

// diagnose gathers the evidence a permission fault needs and establishes how
// much of the store it covers.
//
// Three things, in the order they answer the question:
//
//  1. The parent chain from the store root down to the refused path, with each
//     level's mode. A directory created without its search bit — which is what
//     #151 turned out to be — is invisible in the error text and obvious here.
//  2. The process umask, since a directory created with the wrong mode was
//     created by SOMETHING, and the umask is the only thing that silently
//     changes what a mode means.
//  3. Whether an unrelated directory can be created — under the blob tree, and
//     under the store root. That is what separates one poisoned shard from a
//     store nothing can write to, and it is two syscalls.
func (s *FS) diagnose(path string) (Scope, string) {
	var b strings.Builder
	fmt.Fprintf(&b, "  store diagnosis (#151)\n    root: %s\n", s.root)
	fmt.Fprintf(&b, "    umask: %s\n", probeUmask())

	b.WriteString("    parent chain:\n")
	for _, level := range chain(s.root, path) {
		fmt.Fprintf(&b, "      %s\n", level)
	}

	blobTree := filepath.Join(s.root, blobsDir, hashing.Algorithm)
	blobProbe := probeWrite(blobTree)
	rootProbe := probeWrite(s.root)
	fmt.Fprintf(&b, "    an unrelated directory under %s: %s\n", filepath.Join(blobsDir, hashing.Algorithm), blobProbe)
	fmt.Fprintf(&b, "    an unrelated directory under the store root: %s\n", rootProbe)

	scope := ScopePath
	switch {
	case rootProbe.refused():
		scope = ScopeStore
	case blobProbe.refused():
		scope = ScopeBlobTree
	case blobProbe.inconclusive() || rootProbe.inconclusive():
		scope = ScopeUnknown
	}
	fmt.Fprintf(&b, "    scope: %s", scope)
	return scope, b.String()
}

// chain describes every level from the store root down to path.
//
// Levels are reported even when absent: "which levels exist" is half the
// answer, and a chain that stops existing partway through says the fault is at
// the last level that does.
func chain(root, path string) []string {
	var parts []string
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		parts = append(parts, p)
		if p == root || filepath.Dir(p) == p || len(p) <= len(root) {
			break
		}
	}
	out := make([]string, 0, len(parts))
	for i := len(parts) - 1; i >= 0; i-- {
		p := parts[i]
		label := p
		if p != root {
			label = filepath.Base(p)
		}
		info, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			out = append(out, fmt.Sprintf("%-24s absent", label))
		case err != nil:
			out = append(out, fmt.Sprintf("%-24s unreadable: %v", label, err))
		default:
			out = append(out, fmt.Sprintf("%-24s %v %s%s", label, info.Mode().Perm(), owner(info), searchable(info)))
		}
	}
	return out
}

// searchable names the failure directly rather than leaving a reader to decode
// a mode. A directory without its owner search bit cannot be entered by anyone,
// including the process that created it, and that is exactly the state a umask
// of 0o177 leaves behind.
func searchable(info fs.FileInfo) string {
	if !info.IsDir() {
		return ""
	}
	switch {
	case info.Mode().Perm()&0o100 == 0:
		return "  <- a directory with no owner search bit: nothing can be created inside it"
	case info.Mode().Perm()&0o200 == 0:
		return "  <- a directory with no owner write bit: nothing can be created inside it"
	}
	return ""
}

// probeResult is what happened when the diagnosis tried to create a directory
// somewhere unrelated to the write that failed.
type probeResult struct {
	err error
}

func (p probeResult) refused() bool { return p.err != nil && errors.Is(p.err, fs.ErrPermission) }

// inconclusive is a probe that failed for a reason other than permissions —
// the directory it was aimed at does not exist, say. It says nothing about the
// fault either way, and must not be read as if it did.
func (p probeResult) inconclusive() bool { return p.err != nil && !p.refused() }

func (p probeResult) String() string {
	if p.err == nil {
		return "created (so this level is writable)"
	}
	return fmt.Sprintf("refused: %v", p.err)
}

// probeWrite tries to create and remove a directory under dir.
//
// This is the cheap question the sixth sighting of #151 could not answer: is
// the next write going to fail too? A probe under a DIFFERENT name than the one
// that just failed distinguishes a fault in one shard from a fault in the tree
// that holds every shard.
func probeWrite(dir string) probeResult {
	p := filepath.Join(dir, fmt.Sprintf(".heyarr-probe-%d-%d", os.Getpid(), probeSeq.Add(1)))
	if err := os.Mkdir(p, dirPerm); err != nil {
		return probeResult{err: err}
	}
	_ = os.Remove(p)
	return probeResult{}
}

// probeUmask reports the process umask without changing it.
//
// Reading the umask means setting it — there is no getter — and this store runs
// inside a process where another goroutine may be creating files at the same
// moment, which is the whole subject of #151. So it is inferred instead: create
// a directory asking for 0o777 somewhere that is known to be writable and see
// what the kernel actually gave.
func probeUmask() string {
	dir, err := os.MkdirTemp("", "heyarr-umask")
	if err != nil {
		return fmt.Sprintf("unknown: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	probe := filepath.Join(dir, "p")
	// 0o777 is the point: the umask can only be read back from the bits it
	// took away, so the probe has to ask for all of them. It is created inside
	// a directory MkdirTemp made 0o700 and removed immediately, so nothing else
	// can reach it however permissive it comes out.
	if err := os.Mkdir(probe, 0o777); err != nil { // #nosec G301 -- inferring the umask requires asking for every bit; the parent is private
		return fmt.Sprintf("unknown: %v", err)
	}
	info, err := os.Stat(probe)
	if err != nil {
		return fmt.Sprintf("unknown: %v", err)
	}
	granted := info.Mode().Perm()
	return fmt.Sprintf("%04o (a directory asked for 0777 was granted %04o)", 0o777&^granted, granted)
}
