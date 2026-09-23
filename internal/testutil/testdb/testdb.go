// Package testdb hands tests a fully-migrated controller database without
// replaying every migration per test.
//
// It lives beneath testutil rather than in it so that the many packages which
// import testutil only for golden files — including internal/domain, which may
// not reach persistence — do not link the SQLite engine and goose to get them.
package testdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// template builds the fully-migrated schema once per test binary.
//
// Replaying every migration per test is what made the database-heavy packages
// the slowest in the tree: sqlite.Open + sqlite.Migrate is tens of
// milliseconds normally but seconds under -race, because the race detector
// instruments every access in the pure-Go SQLite engine. It also grows with
// every migration added, silently taxing every package that opens a database.
//
// The template is built by sqlite.Migrate itself, so each test still gets
// exactly the schema Migrate produces — not a second, differently-built one.
// DB.Close checkpoints the WAL with TRUNCATE, so the main file captured here is
// complete on its own; the check below makes that an assertion rather than an
// assumption, because a copy taken beside a populated -wal would be a silently
// older schema.
//
// The bytes are held in memory rather than left in os.TempDir, so nothing
// outlives the process and no TestMain is needed to clean up.
var template = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "heyarr-testdb-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	ctx := context.Background()
	path := filepath.Join(dir, "template.db")
	db, err := sqlite.Open(ctx, sqlite.Options{Path: path})
	if err != nil {
		return nil, err
	}
	if err := sqlite.Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		return nil, fmt.Errorf("testdb: template WAL still holds %d bytes after close; "+
			"the main file alone is not the migrated schema", fi.Size())
	}
	return os.ReadFile(path)
})

// MigratedPath writes a fresh, fully-migrated database into its own
// t.TempDir() and returns the file's path, for tests that open the database
// themselves (with their own Options) or hand the path to code under test.
//
// Each call gets an independent file: nothing is shared with the template or
// with any other test, so tests using it may run in parallel.
func MigratedPath(t testing.TB) string {
	t.Helper()
	path := sqlite.DataDirFor(t.TempDir())
	WriteMigrated(t, path)
	return path
}

// WriteMigrated writes a fresh, fully-migrated database to path, for tests
// whose database must sit at a particular place — beneath a data directory the
// code under test is also given, say. path must not already hold a database.
func WriteMigrated(t testing.TB, path string) {
	t.Helper()
	b, err := template()
	if err != nil {
		t.Fatalf("testdb: building the migrated template: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("testdb: writing %s: %v", path, err)
	}
}

// Migrated opens a fresh, fully-migrated database, closed when the test ends.
// It is equivalent to sqlite.Open on a new file followed by sqlite.Migrate.
func Migrated(t testing.TB) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: MigratedPath(t)})
	if err != nil {
		t.Fatalf("testdb: opening: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
