package sqlite

import "testing"

// TestMigration77MakesAStoredLanguageASet: a candidate recorded with a single
// language becomes a set of one (#888), and a candidate with no language, or
// one already a set, is left exactly as it was.
func TestMigration77MakesAStoredLanguageASet(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 76)
	seedDesiredFixtures(t, db)
	mustExec(t, db, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES ('d1', 'work', 'w1', NULL, 'q1', 1, '', ?, ?)`, ts, ts)
	rows := map[string]string{
		"single": `{"language":"en","size_bytes":5}`,
		"set":    `{"language":["hi","en"]}`,
		"none":   `{"size_bytes":5}`,
	}
	for id, attrs := range rows {
		mustExec(t, db, `INSERT INTO release_candidates
			(id, desired_item_id, search_id, provider, candidate_id, title, attributes,
			 evaluation, accepted, score, terminal, searched_at, created_at)
			VALUES (?, 'd1', 's1', 'p', ?, '', ?, '{}', 1, 0, 0, ?, ?)`, id, id, attrs, ts, ts)
	}

	migrateTo(t, db, 77)

	want := map[string]string{
		"single": `{"language":["en"],"size_bytes":5}`,
		"set":    `{"language":["hi","en"]}`,
		"none":   `{"size_bytes":5}`,
	}
	for id, w := range want {
		var got string
		if err := db.Reader().QueryRowContext(t.Context(),
			`SELECT attributes FROM release_candidates WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("%s: attributes = %s, want %s", id, got, w)
		}
	}

	if err := MigrateDown(t.Context(), db); err != nil {
		t.Fatalf("MigrateDown from 77: %v", err)
	}
}
