//go:build unix

package cas

import (
	"os"
	"syscall"
	"testing"
)

// sourceLinkCount reports how many names the file at path currently has.
//
// The count lives in syscall.Stat_t, which does not exist on Windows at
// COMPILE time — so a `runtime.GOOS != "windows"` check cannot guard it, and
// `go vet ./...` fails on that platform however the branch is written. Split by
// build tag instead, the way hardlinkoutlook_mountns_linux_test.go already
// does for its mount-namespace work.
//
// Nlink is a different width per platform (uint16 on darwin, wider on linux),
// hence the conversion.
func sourceLinkCount(t *testing.T, path string) (uint64, bool) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}
