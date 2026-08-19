package sqlite

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openUnmigrated(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// The acceptance criterion for M1-03: empty → head → zero.
func TestMigrateUpThenAllTheWayDown(t *testing.T) {
	db := openUnmigrated(t)
	ctx := t.Context()

	v, err := SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Fatalf("fresh database is at version %d, want 0", v)
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	head, err := SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if head == 0 {
		t.Fatal("Migrate left the database at version 0")
	}

	var name string
	err = db.Reader().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='schema_meta'`).Scan(&name)
	if err != nil {
		t.Fatalf("migration did not create its table: %v", err)
	}

	migrateAllTheWayDown(t, db)
	back, err := SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if back != 0 {
		t.Errorf("after rolling everything back the database is at version %d, want 0", back)
	}
	// A Down that does not actually drop what Up created is the usual way a
	// rollback "succeeds" while leaving the database unusable.
	err = db.Reader().QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='schema_meta'`).Scan(&name)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("schema_meta survived the rollback (err = %v)", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openUnmigrated(t)
	ctx := t.Context()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	first, err := SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	second, err := SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("version moved from %d to %d on a no-op migrate", first, second)
	}
}

// The safety-critical one. A downgraded binary writing against a newer schema
// does not fail loudly — it writes plausible rows that violate constraints the
// newer schema depends on, and by the time anyone notices, the damage predates
// every backup they still have.
func TestMigrateRefusesADatabaseFromANewerBinary(t *testing.T) {
	db := openUnmigrated(t)
	ctx := t.Context()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	known, err := maxKnownVersion()
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a newer build having migrated this database.
	future := known + 1
	if err := db.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, future)
		return err
	}); err != nil {
		t.Fatalf("seeding a future schema version: %v", err)
	}

	err = Migrate(ctx, db)
	if err == nil {
		t.Fatal("Migrate accepted a database from a newer binary")
	}
	if !errors.Is(err, ErrSchemaNewerThanBinary) {
		t.Errorf("error = %v, want ErrSchemaNewerThanBinary", err)
	}
	for _, want := range []string{"upgrade rather than downgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to say what to do about it", err)
		}
	}
}

// maxKnownVersion is derived from the embedded filenames so it cannot drift
// from what actually ships in the binary.
func TestMaxKnownVersionTracksTheEmbeddedMigrations(t *testing.T) {
	v, err := maxKnownVersion()
	if err != nil {
		t.Fatalf("maxKnownVersion: %v", err)
	}
	if v <= 0 {
		t.Errorf("maxKnownVersion = %d, want a positive version", v)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var sqlFiles int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			sqlFiles++
		}
	}
	if sqlFiles == 0 {
		t.Error("no migrations are embedded — the binary would create an empty database")
	}
}

// migrateAllTheWayDown rolls back until the database is at version 0.
//
// It loops on the version rather than counting steps: a version NUMBER is not a
// migration COUNT, and treating them as interchangeable breaks the moment two
// branches take non-contiguous numbers — which is exactly how this was found.
func migrateAllTheWayDown(t *testing.T, db *DB) {
	t.Helper()
	for range 100 {
		v, err := SchemaVersion(t.Context(), db)
		if err != nil {
			t.Fatal(err)
		}
		if v == 0 {
			return
		}
		if err := MigrateDown(t.Context(), db); err != nil {
			t.Fatalf("MigrateDown from version %d: %v", v, err)
		}
	}
	t.Fatal("still not at version 0 after 100 rollbacks")
}
