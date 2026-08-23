package sqlite

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
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

// migrateTo brings a database up to exactly one migration and stops there, so
// a test can stand up a database as an older build left it.
func migrateTo(t *testing.T, db *DB, version int64) {
	t.Helper()
	if err := configureGoose(); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(t.Context(), db.Writer(), "migrations", version); err != nil {
		t.Fatalf("migrating to %d: %v", version, err)
	}
	got, err := SchemaVersion(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if got != version {
		t.Fatalf("database is at version %d, want %d", got, version)
	}
}

// Migration 00029 applied to a Milestone-1-era database (M5-03).
//
// # Why this one gets its own test
//
// 00029 is the first migration in this repository that DROPS a column other
// migrations' rollbacks re-add, and the column it drops — blobs.chunked — has
// existed since 00002_core.sql and is present in every deployment that has
// ever run. A migration that only ever runs against a fresh database is a
// migration nobody has tested; the databases that matter are the ones with
// rows in them.
//
// The row seeded below is written through the Milestone-1 schema, with the
// boolean, exactly as an installation from that era holds it.
func TestMigrationsUpgradeAMilestoneOneEraDatabase(t *testing.T) {
	db := openUnmigrated(t)
	ctx := t.Context()

	// The schema as Milestone 1 left it: 00002_core.sql created `blobs` with
	// `chunked INTEGER NOT NULL DEFAULT 0`.
	migrateTo(t, db, 2)

	const (
		hash = "blake3:" + "00000000000000000000000000000000000000000000000000000000000000ab"
		ts   = "2026-01-01T00:00:00Z"
	)
	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO blobs (hash, size, mime, chunked, first_seen_at) VALUES (?, 4096, ?, 0, ?)`,
		hash, "video/x-matroska", ts); err != nil {
		t.Fatalf("seeding a Milestone-1-era blob: %v", err)
	}

	// Now upgrade the way a running installation would: all the way to head.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("upgrading a Milestone-1-era database: %v", err)
	}

	// The row survived, with everything that was true about it.
	var (
		size int64
		mime string
		seen string
	)
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT size, mime, first_seen_at FROM blobs WHERE hash = ?`, hash).
		Scan(&size, &mime, &seen); err != nil {
		t.Fatalf("the Milestone-1-era blob did not survive the upgrade: %v", err)
	}
	if size != 4096 || mime != "video/x-matroska" || seen != ts {
		t.Errorf("the row came back as size=%d mime=%q first_seen_at=%q", size, mime, seen)
	}

	// The lying column is gone.
	var legacy int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('blobs') WHERE name = 'chunked'`).
		Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Error("blobs.chunked survived migration 00029")
	}

	// The new tables are there.
	for _, table := range []string{"chunk_manifests", "manifest_chunks", "local_chunks"} {
		var n int
		if err := db.Reader().QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("migration 00029 did not create %s", table)
		}
	}

	// And the pre-existing row reads as UNDECIDED, not as a decision nobody
	// took. Every row in every deployment carried chunked = 0; reading those
	// zeroes as "these bytes never need a manifest" would fabricate a policy
	// decision for the entire installed base — which is the collapse of the
	// third state that migration 00029 exists to undo.
	var state string
	if err := db.Reader().QueryRowContext(ctx, `
		SELECT CASE
		           WHEN m.blob_hash IS NOT NULL              THEN 'present'
		           WHEN b.chunking_exempt_reason IS NOT NULL THEN 'not_required'
		           ELSE 'undecided'
		       END
		FROM blobs b
		LEFT JOIN chunk_manifests m ON m.blob_hash = b.hash
		WHERE b.hash = ?`, hash).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "undecided" {
		t.Errorf("a Milestone-1-era blob reads as %q after the upgrade, want \"undecided\"", state)
	}

	// Integrity, because a DROP COLUMN that leaves a CHECK referring to a
	// column that is not there produces a table that opens and then fails on
	// the first write.
	var integrity string
	if err := db.Reader().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Errorf("integrity_check = %q after the upgrade", integrity)
	}

	// The upgraded database still takes writes through the new shape.
	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO chunk_manifests
			(blob_hash, algorithm, min_size, avg_size, max_size, chunk_count,
			 covered_size, digest, generated_at)
		 VALUES (?, 'fastcdc', 262144, 1048576, 4194304, 1, 4096, ?, ?)`,
		hash, "blake3:"+strings.Repeat("c", 64), ts); err != nil {
		t.Fatalf("writing a manifest against the upgraded database: %v", err)
	}

	// And it still rolls all the way back, which is what makes 00029's Down
	// more than a comment.
	migrateAllTheWayDown(t, db)
}
