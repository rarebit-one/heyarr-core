//go:build !windows

package cas

import (
	"os"
	"syscall"
)

func deviceOf(path string) (int64, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false, nil
	}
	return int64(st.Dev), true, nil // #nosec G115 -- a device number, never used as a size
}
