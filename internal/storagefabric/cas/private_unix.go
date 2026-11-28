//go:build linux || darwin

package cas

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"github.com/rarebit-one/void-which-binds-go/hashing"
)

// CheckPrivateBlob checks link count and digest using the same open descriptor,
// then confirms the content path still names that inode. An external process
// retaining a preexisting writable file descriptor is outside this proof and
// remains governed by the store's immutability/integrity model.
func (s *FS) CheckPrivateBlob(ctx context.Context, h hashing.Hash) error {
	return s.checkPrivateBlob(ctx, h, nil)
}

// afterHash is a test seam for an inode swap at the hash/path boundary.
func (s *FS) checkPrivateBlob(ctx context.Context, h hashing.Hash, afterHash func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := s.blobPath(h)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return err
	}
	if err := privateFileInfo(before); err != nil {
		return err
	}
	if _, err := hashing.Verify(&ctxReader{ctx: ctx, r: f}, h); err != nil {
		return fmt.Errorf("cas: private blob digest: %w", err)
	}
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if err := privateFileInfo(after); err != nil {
		return err
	}
	if afterHash != nil {
		afterHash()
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || !os.SameFile(after, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("cas: blob changed during private verification")
	}
	return nil
}
func privateFileInfo(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Mode().Perm()&0222 != 0 {
		return fmt.Errorf("cas: blob has mutable or unproven external links")
	}
	return nil
}
