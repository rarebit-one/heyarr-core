package sqlite

import (
	"strings"
	"testing"
)

// TestMigration75KeepsOutcomesAndAdmitsReports: rebuilding catalog_ops to
// admit `unsourced` keeps both tables that cascade from it, fan-out and hold
// outcomes, and their references still resolve to the rebuilt table.
func TestMigration75KeepsOutcomesAndAdmitsReports(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 74)
	insertCatalogOp(t, db, "sha256:acq", "acquire", "[]")
	insertCatalogOp(t, db, "sha256:hold", "hold", "[]")
	for _, q := range []string{
		`INSERT INTO catalog_op_applications (op_hash, outcome, detail, applied_at)
		 VALUES ('sha256:acq', 'no_site_source', 'from site north', '2026-11-17T00:00:00Z')`,
		`INSERT INTO catalog_hold_applications (op_hash, blob_hash, outcome, applied_at)
		 VALUES ('sha256:hold', 'blake3:` + strings.Repeat("a", 64) + `', 'pulling', '2026-11-17T00:00:00Z')`,
	} {
		if _, err := db.Writer().ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	migrateTo(t, db, 75)

	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.Reader().QueryRowContext(t.Context(), q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(`SELECT count(*) FROM catalog_op_applications`) != 1 || count(`SELECT count(*) FROM catalog_hold_applications`) != 1 {
		t.Fatal("the rebuild deleted recorded outcomes")
	}
	insertCatalogOp(t, db, "sha256:uns", "unsourced", `["sha256:acq"]`)
	if _, err := db.Writer().ExecContext(t.Context(),
		`DELETE FROM catalog_ops WHERE op_hash IN ('sha256:acq', 'sha256:hold')`); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM catalog_op_applications`)+count(`SELECT count(*) FROM catalog_hold_applications`) != 0 {
		t.Fatal("the outcome tables no longer reference the rebuilt catalog_ops")
	}
	if _, err := db.Writer().ExecContext(t.Context(), `
		INSERT INTO catalog_ops (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
		VALUES ('sha256:bad', 'movie', 'x', 'need', 's', '[]', 'x', 't', 'x')`); err == nil {
		t.Fatal("the CHECK admitted an unknown kind")
	}
	var fks int
	if err := db.Writer().QueryRowContext(t.Context(), `PRAGMA foreign_keys`).Scan(&fks); err != nil {
		t.Fatal(err)
	}
	if fks != 1 {
		t.Fatal("the migration left foreign keys off on the writer")
	}
}

// TestMigration75DownDropsUncitedReports: rolling back drops reports nothing
// cites (the sibling re-sends them) and keeps every other op.
func TestMigration75DownDropsUncitedReports(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 75)
	insertCatalogOp(t, db, "sha256:acq", "acquire", "[]")
	insertCatalogOp(t, db, "sha256:uns", "unsourced", `["sha256:acq"]`)
	insertCatalogOp(t, db, "sha256:hold", "hold", `["sha256:acq"]`)
	if err := MigrateDown(t.Context(), db); err != nil {
		t.Fatalf("MigrateDown from 75: %v", err)
	}
	if got := countCatalogOps(t, db); got != 2 {
		t.Fatalf("after rollback catalog_ops has %d rows, want the acquire and the hold", got)
	}
}

// TestMigration75DownRefusesToStrandACitedReport: a hold citing a report would
// lose its past, so the rollback refuses before changing anything.
func TestMigration75DownRefusesToStrandACitedReport(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 75)
	insertCatalogOp(t, db, "sha256:acq", "acquire", "[]")
	insertCatalogOp(t, db, "sha256:uns", "unsourced", `["sha256:acq"]`)
	insertCatalogOp(t, db, "sha256:hold", "hold", `["sha256:uns"]`)
	err := MigrateDown(t.Context(), db)
	if err == nil {
		t.Fatal("the rollback dropped a report another op cites")
	}
	if !strings.Contains(err.Error(), "catalog_ops_down_guard") {
		t.Fatalf("refusal does not name the guard: %v", err)
	}
	if got := countCatalogOps(t, db); got != 3 {
		t.Fatalf("a refused rollback left %d ops, want all three", got)
	}
}
