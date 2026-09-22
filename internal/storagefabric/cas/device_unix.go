//go:build !windows

package cas

import (
	"os"
	"syscall"
)

func deviceOf(path string) (int64, bool, error) {
	// #nosec G703 -- path is a configured root (cas.root, a library root, a
	// download path), not a request parameter; and a stat of it reads no
	// content. The taint arrives from HardlinkOutlook's arguments, which come
	// from the same configuration.
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
