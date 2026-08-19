//go:build linux

package cas

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// reflink asks the filesystem to clone src into dst copy-on-write.
//
// FICLONE is supported by btrfs, XFS with reflink=1, and ZFS 2.2+ with block
// cloning enabled. Where it is not, the error is wrapped in errDegrade so Link
// falls through to a hardlink and then a copy — an unsupported filesystem is
// ordinary, not a failure (ADR-0014).
func reflink(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- src comes from a configured library root
	if err != nil {
		return fmt.Errorf("cas: opening %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	// #nosec G304 -- dst is derived from a validated hash inside the CAS root
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, blobPerm)
	if err != nil {
		return fmt.Errorf("%w: creating %s: %w", errDegrade, dst, err)
	}
	defer func() { _ = out.Close() }()

	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		// Leave nothing behind for the next rung to trip over.
		_ = os.Remove(dst)
		return fmt.Errorf("%w: FICLONE on %s: %w", errDegrade, dst, err)
	}
	return nil
}
