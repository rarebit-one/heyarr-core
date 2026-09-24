package sqlite

import (
	"path/filepath"
	"testing"
)

func TestAppliedSchemaVersionReportsZeroBeforeAnyMigration(t *testing.T) {
	db := openUnmigratedDB(t)
	version, err := AppliedSchemaVersion(t.Context(), db)
	if err != nil {
		t.Fatalf("AppliedSchemaVersion: %v", err)
	}
	if version != 0 {
		t.Fatalf("an unmigrated database reported version %d, want 0", version)
	}
}

func TestAppliedSchemaVersionAgreesWithGoose(t *testing.T) {
	db := openTestDB(t) // already migrated
	want, err := SchemaVersion(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	got, err := AppliedSchemaVersion(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("AppliedSchemaVersion = %d, goose says %d", got, want)
	}
	if want == 0 {
		t.Fatal("a migrated database reported version 0 — this test would pass on a broken reader")
	}
}

// The regression, stated as the property rather than as the race.
//
// Roles start concurrently (ADR-0002) and only the controller migrates (§7),
// but a worker still wants to know whether the schema is ready. Asking goose
// that question CREATES its bookkeeping table — so two processes race on the
// CREATE and one loses with "table goose_db_version already exists". In
// practice the controller loses, because it is doing more work, which turns a
// harmless status check by a worker into a failed startup for the role that
// owns the schema.
//
// scripts/acceptance.sh is what caught this, because the race needs two
// processes and this test suite has one. Reproducing the race here is not
// possible; asserting the mechanism that caused it is, and it is the stronger
// assertion anyway: a status check must not write.
func TestAStatusCheckCreatesNothing(t *testing.T) {
	db := openUnmigratedDB(t)

	before := tableNames(t, db)
	if _, err := AppliedSchemaVersion(t.Context(), db); err != nil {
		t.Fatalf("AppliedSchemaVersion: %v", err)
	}
	after := tableNames(t, db)

	if len(after) != len(before) {
		t.Fatalf("checking the version created tables: %v then %v", before, after)
	}
	if _, ok := after["goose_db_version"]; ok {
		t.Fatal("checking the schema version created goose_db_version — " +
			"that CREATE races the controller's migration, and the controller is the one that loses")
	}
}

func TestAStatusCheckStillWorksOnAMigratedDatabase(t *testing.T) {
	db := openTestDB(t) // already migrated
	before := tableNames(t, db)
	if _, err := AppliedSchemaVersion(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if after := tableNames(t, db); len(after) != len(before) {
		t.Fatalf("checking the version changed the schema: %v then %v", before, after)
	}
}

func tableNames(t *testing.T, db *DB) map[string]struct{} {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	names := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// openUnmigratedDB is openTestDB without the migration, because two of these
// tests are about what happens BEFORE the schema exists.
func openUnmigratedDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestUnappliedMigrationsIsEveryKnownMigrationBeforeAnyMigrate(t *testing.T) {
	db := openUnmigratedDB(t)
	before := tableNames(t, db)
	missing, err := UnappliedMigrations(t.Context(), db)
	if err != nil {
		t.Fatalf("UnappliedMigrations: %v", err)
	}
	known, err := knownVersions()
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != len(known) {
		t.Fatalf("an unmigrated database is missing %d migrations, want all %d", len(missing), len(known))
	}
	// A role that does not own the schema asks this; it must not write either
	// (see TestAStatusCheckCreatesNothing).
	if after := tableNames(t, db); len(after) != len(before) {
		t.Fatalf("asking what is unapplied changed the schema: %v then %v", before, after)
	}
}

func TestUnappliedMigrationsIsEmptyOnceMigrated(t *testing.T) {
	db := openTestDB(t) // already migrated
	missing, err := UnappliedMigrations(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("a fully migrated database reports unapplied migrations %v", missing)
	}
}

// The case a version number cannot see: a database at the highest known
// version that has not yet applied a gap-filler. AppliedSchemaVersion says it
// is current; it is missing a table.
func TestUnappliedMigrationsSeesAMissingGapFiller(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t)
	if _, err := db.Writer().ExecContext(ctx, `DELETE FROM goose_db_version WHERE version_id = 22`); err != nil {
		t.Fatal(err)
	}
	known, err := KnownSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v, err := AppliedSchemaVersion(ctx, db); err != nil || v != known {
		t.Fatalf("precondition: AppliedSchemaVersion = %d, %v; want %d", v, err, known)
	}
	missing, err := UnappliedMigrations(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != 22 {
		t.Fatalf("UnappliedMigrations = %v, want [22]", missing)
	}
}

func TestUnappliedMigrationsSeesARollback(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t)
	if err := MigrateDown(ctx, db); err != nil {
		t.Fatal(err)
	}
	known, err := KnownSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	missing, err := UnappliedMigrations(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != known {
		t.Fatalf("after rolling back the head, UnappliedMigrations = %v, want [%d]", missing, known)
	}
}
