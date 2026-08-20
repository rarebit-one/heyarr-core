//go:build windows

package scanner

import "os"

// deviceAndInode reports zeroes on Windows, where syscall.Stat_t does not
// exist and the file index is not exposed through os.FileInfo.
//
// The fingerprint therefore falls back to (size, mtime_ns) on this platform —
// see Fingerprint.Unchanged. That is weaker: a replacement file of identical
// length with a preserved mtime reads as unchanged. It is also exactly what
// every *arr scanner on Windows already does, and the alternative is opening
// and hashing every file on every scan.
func deviceAndInode(os.FileInfo) (dev, inode int64) { return 0, 0 }
