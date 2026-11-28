package sqlite

import "testing"

func TestMigration80RefusesDowngradeWithRecordingWorks(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 80)
	if _, err := db.Writer().ExecContext(t.Context(), `INSERT INTO works
		(id,content_type,work_key,title,sort_title,created_at,updated_at)
		VALUES ('recording','music','music-recording:11111111-1111-4111-8111-111111111111',
		'Jump','jump','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateDown(t.Context(), db); err == nil {
		t.Fatal("downgrade allowed a recording work to survive for an older binary")
	}
	if version, err := SchemaVersion(t.Context(), db); err != nil || version != 80 {
		t.Fatalf("after refused downgrade version = %d, err = %v; want 80", version, err)
	}
	var count int
	if err := db.Reader().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM works WHERE id='recording'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recording count = %d, err = %v; want 1", count, err)
	}
	if _, err := db.Writer().ExecContext(t.Context(), `DELETE FROM works WHERE id='recording'`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateDown(t.Context(), db); err != nil {
		t.Fatalf("downgrade after removal: %v", err)
	}
	if version, err := SchemaVersion(t.Context(), db); err != nil || version != 79 {
		t.Fatalf("after retry version = %d, err = %v; want 79", version, err)
	}
}

func TestMigration80AllowsDowngradeWithOrdinaryMusic(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 80)
	if _, err := db.Writer().ExecContext(t.Context(), `INSERT INTO works
		(id,content_type,work_key,title,sort_title,created_at,updated_at)
		VALUES ('album','music','music:recording:album','Album','album',
		'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateDown(t.Context(), db); err != nil {
		t.Fatalf("ordinary artist named Recording should not block downgrade: %v", err)
	}
	if version, err := SchemaVersion(t.Context(), db); err != nil || version != 79 {
		t.Fatalf("version = %d, err = %v; want 79", version, err)
	}
}
