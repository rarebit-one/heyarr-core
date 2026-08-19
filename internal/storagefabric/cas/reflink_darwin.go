//go:build darwin

package cas

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// reflink asks APFS to clone src into dst copy-on-write.
//
// clonefile requires that dst does not exist, and it copies metadata along with
// the data. Where cloning is unavailable — a non-APFS volume, or across
// filesystems — the error is wrapped in errDegrade so Link falls through to a
// hardlink and then a copy (ADR-0014).
func reflink(src, dst string) error {
	if err := unix.Clonefile(src, dst, 0); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("%w: clonefile %s: %w", errDegrade, dst, err)
	}
	// clonefile copies the source's permissions; blobs are read-only.
	if err := os.Chmod(dst, blobPerm); err != nil {
		return fmt.Errorf("cas: setting blob mode: %w", err)
	}
	return nil
}
