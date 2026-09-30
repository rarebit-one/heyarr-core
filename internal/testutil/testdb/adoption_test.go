package testdb_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// migrateAllowed are the test files that must run the real migrations rather
// than copy the template: the migration tests themselves, tests that assert on
// migration state, and this package, which builds the template.
var migrateAllowed = []string{
	"internal/persistence/sqlite/",
	"internal/testutil/testdb/",
	"internal/worker/schema_test.go",
}

// No other test replays the migrations per test (#610). That tax is invisible
// in a profile — every test in the package just reads as evenly slow — and it
// grows with every migration merged, which is how it walked race-nightly into
// its timeout (#602). #620/#630 moved 71 call sites onto the template and a
// new one had arrived within days; this makes the next one fail here instead.
func TestNoTestReplaysTheMigrations(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	needle := []byte("sqlite.Migrate(")
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".worktrees", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, allowed := range migrateAllowed {
			if strings.HasPrefix(rel, allowed) {
				return nil
			}
		}
		b, err := os.ReadFile(path) // #nosec G304 -- a test file inside this module
		if err != nil {
			return err
		}
		if bytes.Contains(b, needle) {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offenders {
		t.Errorf("%s calls sqlite.Migrate: use testdb.Migrated, or testdb.WriteMigrated "+
			"then sqlite.Open where the file must sit at a given path "+
			"(or add it to migrateAllowed if it genuinely tests migrations)", o)
	}
}

// moduleRoot walks up from the package directory to the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
