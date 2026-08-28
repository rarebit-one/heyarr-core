//go:build linux

package cas

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SameMount reports whether two paths sit in the same mount — the question
// link(2) actually asks. link returns EXDEV across different mounts even when
// they share a device, which is the #222 case: under ProtectSystem=strict the
// unit sees the read-only library and the read-write store as SEPARATE bind
// mounts of one filesystem, so st_dev matches but a hardlink cannot cross them.
// The mount id can see the difference that st_dev cannot.
//
// Its shape matches SameFilesystem so the callers are a one-word swap: the
// second return is whether the answer is known (false when mountinfo is
// unreadable), and the caller must not read "unknown" as "different".
func SameMount(a, b string) (same, known bool, err error) {
	idA, okA, err := mountIDOf(a)
	if err != nil {
		return false, false, fmt.Errorf("cas: examining %s: %w", a, err)
	}
	idB, okB, err := mountIDOf(b)
	if err != nil {
		return false, false, fmt.Errorf("cas: examining %s: %w", b, err)
	}
	if !okA || !okB {
		return false, false, nil
	}
	return idA == idB, true, nil
}

// mountIDOf returns the mount id of the mount that contains path, from
// /proc/self/mountinfo. The mount id (field 1) uniquely identifies a vfsmount,
// so a hardlink between two paths with the same mount id can succeed.
func mountIDOf(path string) (string, bool, error) {
	real, err := existingAncestor(path)
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		// No mountinfo (unusual on Linux). Unknown, not "different".
		return "", false, nil
	}
	id, ok := mountIDForPath(data, real)
	return id, ok, nil
}

// existingAncestor resolves path to an absolute, symlink-free form and, if it
// does not exist yet, walks up to the nearest ancestor that does — a path's
// mount is a property of the deepest existing directory at or above it.
func existingAncestor(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for {
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			return resolved, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return abs, nil // reached the root
		}
		abs = parent
	}
}

// mountIDForPath finds, among the mountinfo lines, the mount whose mount point
// is the longest path-component prefix of path, and returns its mount id. Pure
// so it can be tested against a fixture rather than the live /proc.
func mountIDForPath(mountinfo []byte, path string) (string, bool) {
	bestID := ""
	bestLen := -1
	sc := bufio.NewScanner(bytes.NewReader(mountinfo))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// mountinfo fields: id(0) parent(1) major:minor(2) root(3) mountpoint(4) …
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mountPoint := unescapeMountField(fields[4])
		if pathHasPrefix(path, mountPoint) && len(mountPoint) > bestLen {
			bestID = fields[0]
			bestLen = len(mountPoint)
		}
	}
	if bestLen < 0 {
		return "", false
	}
	return bestID, true
}

// pathHasPrefix reports whether path is at or below dir, comparing whole path
// components so /srv/media is a prefix of /srv/media/x but not /srv/mediafoo.
func pathHasPrefix(path, dir string) bool {
	if dir == "/" || path == dir {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

// unescapeMountField reverses the octal escaping the kernel applies to space,
// tab, newline and backslash in mountinfo fields (\040 \011 \012 \134).
func unescapeMountField(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			// bitSize 8 rejects any escape above \377, so the value provably
			// fits a byte; the kernel only ever escapes \040 \011 \012 \134.
			if o, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(o)) // #nosec G115 -- ParseUint bitSize 8 bounds o to 0-255
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
