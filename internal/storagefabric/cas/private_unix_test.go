//go:build linux || darwin

package cas

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPrivateBlobRejectsPathSwapAfterHash(t *testing.T) {
	s := newStore(t)
	blob := put(t, s, []byte("known bytes"))
	changed := false
	err := s.checkPrivateBlob(context.Background(), blob.Hash, func() {
		path := s.blobPath(blob.Hash)
		replacement := filepath.Join(filepath.Dir(path), "replacement")
		if err := os.WriteFile(replacement, []byte("known bytes"), 0440); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
		changed = true
	})
	if !changed || err == nil || !strings.Contains(err.Error(), "changed during") {
		t.Fatalf("path swap not rejected: %v", err)
	}
}
