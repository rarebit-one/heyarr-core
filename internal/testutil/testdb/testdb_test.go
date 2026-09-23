package testdb_test

import (
	"context"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// A copy is at the schema this binary ships — the template is not a stale or
// partial migration — and running Migrate over it is the no-op it would be on
// any fully-migrated database.
func TestMigratedIsAtTheKnownSchemaVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testdb.Migrated(t)

	known, err := sqlite.KnownSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	got, err := sqlite.SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if got != known {
		t.Fatalf("schema version = %d, want %d", got, known)
	}
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrating an already-migrated copy: %v", err)
	}
}

// Two copies share nothing: a write to one is invisible to the other.
func TestMigratedCopiesAreIndependent(t *testing.T) {
	t.Parallel()
	a, b := testdb.Migrated(t), testdb.Migrated(t)
	if a.Path() == b.Path() {
		t.Fatalf("both copies at %s", a.Path())
	}
	if _, err := a.Writer().Exec(`CREATE TABLE only_in_a (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.Reader().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE name = 'only_in_a'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a table created in one copy is visible in the other")
	}
}
