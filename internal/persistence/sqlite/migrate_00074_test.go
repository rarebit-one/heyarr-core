package sqlite

import (
	"strings"
	"testing"
)

// TestMigration74KeepsFanOutOutcomes: rebuilding catalog_ops to admit `hold`
// must not take catalog_op_applications with it. It references catalog_ops ON
// DELETE CASCADE, so a rebuild with foreign keys enforced would have deleted
// every recorded fan-out outcome, and the applier would re-apply every acquire.
func TestMigration74KeepsFanOutOutcomes(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 73)
	insertCatalogOp(t, db, "sha256:acq", "acquire", "[]")
	if _, err := db.Writer().ExecContext(t.Context(), `
		INSERT INTO catalog_op_applications (op_hash, outcome, detail, applied_at)
		VALUES ('sha256:acq', 'applied', 'from site north', '2026-11-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	migrateTo(t, db, 74)

	var n int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT count(*) FROM catalog_op_applications WHERE op_hash = 'sha256:acq'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the rebuild deleted the fan-out outcome (%d rows left)", n)
	}
	// The reference still resolves: deleting the op cascades to its outcome.
	if _, err := db.Writer().ExecContext(t.Context(), `DELETE FROM catalog_ops WHERE op_hash = 'sha256:acq'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM catalog_op_applications`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("catalog_op_applications no longer references the rebuilt catalog_ops")
	}
	var fks int
	if err := db.Writer().QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&fks); err != nil {
		t.Fatal(err)
	}
	if fks != 1 {
		t.Fatal("the migration left foreign keys off on the writer")
	}
}

// TestMigration74AdmitsHoldsAndTheirOutcomes: the new kind and its outcome
// table, with the outcome's CHECKs in force.
func TestMigration74AdmitsHoldsAndTheirOutcomes(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 74)
	insertCatalogOp(t, db, "sha256:hold", "hold", "[]")
	blob := "blake3:" + strings.Repeat("a", 64)
	if _, err := db.Writer().ExecContext(t.Context(), `
		INSERT INTO catalog_hold_applications (op_hash, blob_hash, outcome, applied_at)
		VALUES ('sha256:hold', ?, 'pulling', '2026-11-17T00:00:00Z')`, blob); err != nil {
		t.Fatalf("a hold outcome was refused: %v", err)
	}
	for _, bad := range []string{
		`INSERT INTO catalog_hold_applications (op_hash, blob_hash, outcome, applied_at)
		 VALUES ('sha256:hold2', '` + blob + `', 'applied', 'x')`,
		`INSERT INTO catalog_hold_applications (op_hash, blob_hash, outcome, applied_at)
		 VALUES ('sha256:hold', 'sha256:nope', 'held', 'x')`,
	} {
		if _, err := db.Writer().ExecContext(t.Context(), bad); err == nil {
			t.Fatalf("accepted: %s", bad)
		}
	}
}

// TestMigration74DownDropsUncitedHolds: rolling back drops holds nothing cites
// (the sibling re-sends them) and keeps every other op.
func TestMigration74DownDropsUncitedHolds(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 74)
	insertCatalogOp(t, db, "sha256:hold", "hold", "[]")
	insertCatalogOp(t, db, "sha256:acq", "acquire", "[]")
	insertCatalogOp(t, db, "sha256:del", "delete", `["sha256:acq"]`)
	if err := MigrateDown(t.Context(), db); err != nil {
		t.Fatalf("MigrateDown from 74: %v", err)
	}
	if got := countCatalogOps(t, db); got != 2 {
		t.Fatalf("after rollback catalog_ops has %d rows, want the acquire and the delete", got)
	}
}

// TestMigration74DownRefusesToStrandACitedHold: an op citing a hold would lose
// its past, so the rollback refuses before changing anything.
func TestMigration74DownRefusesToStrandACitedHold(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 74)
	insertCatalogOp(t, db, "sha256:hold", "hold", "[]")
	insertCatalogOp(t, db, "sha256:del", "delete", `["sha256:hold"]`)
	err := MigrateDown(t.Context(), db)
	if err == nil {
		t.Fatal("the rollback dropped a hold another op cites")
	}
	if !strings.Contains(err.Error(), "catalog_ops_down_guard") {
		t.Fatalf("refusal does not name the guard: %v", err)
	}
	if got := countCatalogOps(t, db); got != 2 {
		t.Fatalf("a refused rollback left %d ops, want both", got)
	}
	var tables int
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT count(*) FROM sqlite_master WHERE name = 'catalog_hold_applications'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 1 {
		t.Fatal("a refused rollback dropped the outcome table")
	}
}
