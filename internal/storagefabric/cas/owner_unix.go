//go:build unix

package cas

import (
	"fmt"
	"io/fs"
	"syscall"
)

// owner reports the numeric uid and gid of a path, because "permission denied"
// on a directory whose mode looks fine is usually a directory somebody else
// owns.
func owner(info fs.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("uid=%d gid=%d", st.Uid, st.Gid)
}
