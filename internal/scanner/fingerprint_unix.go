//go:build !windows

package scanner

import (
	"os"
	"syscall"
)

// deviceAndInode reports the file's device and inode numbers.
//
// They are what makes the fingerprint honest about the cases size and mtime
// miss: a file replaced by one of identical length and a preserved mtime (rsync
// -t, a restore from backup, a torrent client rewriting in place) is a
// different inode, and a root that moved to a different filesystem is a
// different device. Both would otherwise read as "unchanged" and never be
// re-ingested.
//
// A zero pair means "not available" and the comparison falls back to
// (size, mtime_ns) — see Fingerprint.Unchanged. That is the Windows build's
// permanent state (fingerprint_windows.go) and it is also what old rows carry,
// since 00002_core.sql defaults both columns to 0.
func deviceAndInode(info os.FileInfo) (dev, inode int64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	// Dev is int32 on darwin and uint64 on linux, and Ino is uint64 on both.
	// SQLite stores signed 64-bit integers, so both are widened to int64 here
	// rather than at the call site.
	return int64(st.Dev), int64(st.Ino) // #nosec G115 -- device and inode numbers are stored, never used as sizes
}
