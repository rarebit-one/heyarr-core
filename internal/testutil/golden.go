package testutil

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// update rewrites golden files instead of comparing against them:
//
//	go test ./... -update
//
// It lives here rather than in each test package so there is exactly one flag
// name, and so packages under internal/domain — which may not import "os" —
// can still use golden files.
var update = flag.Bool("update", false, "rewrite golden files instead of comparing against them")

// Golden compares got against the contents of path, failing the test on any
// difference. With -update it writes got to path and passes.
//
// A golden file regenerated from the code under test proves only that the
// output has not changed; pair it with hand-written expectations for the fields
// that actually carry meaning.
func Golden(t *testing.T, path string, got []byte) {
	t.Helper()

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("golden: create %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("golden: write %s: %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("golden: read %s: %v (run the tests with -update to create it)", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("golden %s: output differs from the committed file.\n"+
			"--- want ---\n%s\n--- got ---\n%s\n"+
			"If the new output is correct, re-run with -update and review the diff.",
			path, want, got)
	}
}
