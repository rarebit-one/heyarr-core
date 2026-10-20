package sqlite

import (
	"strings"
	"testing"
)

// insertCatalogOp writes a raw catalog_ops row; the token is not verified by
// the migration, only the kind and the prev citations matter here.
func insertCatalogOp(t *testing.T, db *DB, hash, kind, prev string) {
	t.Helper()
	if _, err := db.Writer().ExecContext(t.Context(), `
		INSERT INTO catalog_ops (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
		VALUES (?, 'movie', 'movie:x:2000', ?, 'ed25519:test', ?, '2026-10-20T00:00:00Z', 'tok', '2026-10-20T00:00:00Z')`,
		hash, kind, prev); err != nil {
		t.Fatal(err)
	}
}

func countCatalogOps(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM catalog_ops`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestMigration66DownDropsUncitedAcquires: rolling 00066 back drops acquire ops
// nothing cites (they are the sibling's to re-send) and keeps the tombstone
// ops, which still evaluate.
func TestMigration66DownDropsUncitedAcquires(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 66)
	insertCatalogOp(t, db, "sha256:acq", "acquire", "[]")
	insertCatalogOp(t, db, "sha256:del", "delete", "[]")
	if err := MigrateDown(t.Context(), db); err != nil {
		t.Fatalf("MigrateDown from 66: %v", err)
	}
	if got := countCatalogOps(t, db); got != 1 {
		t.Fatalf("after rollback catalog_ops has %d rows, want the delete alone", got)
	}
}

// TestMigration66DownRefusesToStrandACitedAcquire: when a delete cites an
// acquire op in its prev, dropping the acquire would make the delete bad_prev
// and lift its tombstone on the next reconcile. The rollback refuses, and the
// database is left at 66 with every row intact.
func TestMigration66DownRefusesToStrandACitedAcquire(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 66)
	insertCatalogOp(t, db, "sha256:acq", "acquire", "[]")
	insertCatalogOp(t, db, "sha256:del", "delete", `["sha256:acq"]`)
	err := MigrateDown(t.Context(), db)
	if err == nil {
		t.Fatal("the rollback dropped an acquire op a delete cites")
	}
	if !strings.Contains(err.Error(), "catalog_ops_down_guard") {
		t.Fatalf("refusal does not name the guard: %v", err)
	}
	v, verr := SchemaVersion(t.Context(), db)
	if verr != nil {
		t.Fatal(verr)
	}
	if v != 66 {
		t.Fatalf("after a refused rollback the database is at %d, want 66", v)
	}
	if got := countCatalogOps(t, db); got != 2 {
		t.Fatalf("a refused rollback left %d rows, want both", got)
	}
}

// TestMigration66DownRefusalIsRetryable: after a refusal, removing the citing
// op lets the same connection roll back; the guard table does not stand in
// the way of the second attempt.
func TestMigration66DownRefusalIsRetryable(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 66)
	insertCatalogOp(t, db, "sha256:acq", "acquire", "[]")
	insertCatalogOp(t, db, "sha256:del", "delete", `["sha256:acq"]`)
	if err := MigrateDown(t.Context(), db); err == nil {
		t.Fatal("precondition: the first rollback should refuse")
	}
	if _, err := db.Writer().ExecContext(t.Context(), `DELETE FROM catalog_ops WHERE op_hash = 'sha256:del'`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateDown(t.Context(), db); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
}
