package sqlite

import (
	"testing"
	"time"
)

// Pairs of instants, earlier first, that fall in the same second. Each is a
// case trimmed RFC3339Nano gets backwards as TEXT.
var sameSecondPairs = []struct {
	name           string
	earlier, later time.Time
}{
	{
		"a shorter fraction",
		time.Date(2026, 8, 1, 12, 0, 0, 100_000_000, time.UTC), // "…00.1Z"
		time.Date(2026, 8, 1, 12, 0, 0, 150_000_000, time.UTC), // "…00.15Z"
	},
	{
		"a whole second",
		time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC), // "…00Z"
		time.Date(2026, 8, 1, 12, 0, 0, 1, time.UTC), // "…00.000000001Z"
	},
	{
		"a trailing-zero fraction",
		time.Date(2026, 8, 1, 12, 0, 0, 500_000_000, time.UTC), // "…00.5Z"
		time.Date(2026, 8, 1, 12, 0, 0, 500_000_001, time.UTC), // "…00.500000001Z"
	},
}

// The bug, and the fix, asked of SQLite itself: the comparison that matters is
// the one the database makes, not the one Go makes.
func TestFixedWidthTimestampsCompareChronologicallyInSQLite(t *testing.T) {
	db := openUnmigrated(t)
	lessInSQL := func(a, b string) bool {
		t.Helper()
		var less bool
		if err := db.Reader().QueryRow(`SELECT ? < ?`, a, b).Scan(&less); err != nil {
			t.Fatal(err)
		}
		return less
	}

	for _, p := range sameSecondPairs {
		t.Run(p.name, func(t *testing.T) {
			trimmedA, trimmedB := p.earlier.Format(time.RFC3339Nano), p.later.Format(time.RFC3339Nano)
			if lessInSQL(trimmedA, trimmedB) {
				t.Errorf("RFC3339Nano %q < %q in SQLite — this test no longer demonstrates the bug", trimmedA, trimmedB)
			}

			fixedA, fixedB := FormatTimestamp(p.earlier), FormatTimestamp(p.later)
			if !lessInSQL(fixedA, fixedB) {
				t.Errorf("fixed-width %q is not < %q in SQLite, though it is the earlier instant", fixedA, fixedB)
			}
			if len(fixedA) != len(fixedB) {
				t.Errorf("fixed-width layout is not fixed: %q and %q", fixedA, fixedB)
			}
		})
	}
}

func TestFormatTimestampIsUTC(t *testing.T) {
	at := time.Date(2026, 8, 1, 20, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	if got, want := FormatTimestamp(at), "2026-08-01T12:00:00.000000000Z"; got != want {
		t.Errorf("FormatTimestamp = %q, want %q", got, want)
	}
}

// Rows written before the fixed-width layout still hold trimmed values; a
// reader must take both and see the same instant.
func TestParseTimestampReadsBothLayouts(t *testing.T) {
	for _, p := range sameSecondPairs {
		for _, at := range []time.Time{p.earlier, p.later} {
			for _, s := range []string{at.Format(time.RFC3339Nano), FormatTimestamp(at)} {
				got, err := ParseTimestamp(s)
				if err != nil {
					t.Fatalf("ParseTimestamp(%q): %v", s, err)
				}
				if !got.Equal(at) {
					t.Errorf("ParseTimestamp(%q) = %v, want %v", s, got, at)
				}
			}
		}
	}
}

// Migration 00055 rewrites the compared columns already on disk into the
// fixed-width layout, without moving any instant, and leaves alone what it
// does not recognise.
func TestSortableTimestampsMigrationRewritesExistingRows(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 54)

	stored := []string{
		"2026-08-01T12:00:00Z",
		"2026-08-01T12:00:00.1Z",
		"2026-08-01T12:00:00.15Z",
		"2026-08-01T12:00:00.12345678Z",
		"2026-08-01T12:00:00.000000001Z", // already fixed-width
	}
	for i, s := range stored {
		mustExec(t, db, `INSERT INTO jobs (id, type, run_after, created_at, updated_at) VALUES (?, 'x', ?, ?, ?)`,
			string(rune('a'+i)), s, s, s)
	}
	// Not UTC: never sortable, and not ours to reinterpret.
	const offset = "2026-08-01T20:00:00.1+08:00"
	mustExec(t, db, `INSERT INTO jobs (id, type, run_after, created_at, updated_at) VALUES ('z', 'x', ?, ?, ?)`,
		offset, offset, offset)

	if err := Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for i, s := range stored {
		var got string
		if err := db.Reader().QueryRow(`SELECT run_after FROM jobs WHERE id = ?`, string(rune('a'+i))).Scan(&got); err != nil {
			t.Fatal(err)
		}
		want, err := ParseTimestamp(s)
		if err != nil {
			t.Fatal(err)
		}
		if got != FormatTimestamp(want) {
			t.Errorf("%q migrated to %q, want %q", s, got, FormatTimestamp(want))
		}
	}
	var got string
	if err := db.Reader().QueryRow(`SELECT run_after FROM jobs WHERE id = 'z'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != offset {
		t.Errorf("a non-UTC value was rewritten to %q", got)
	}

	// And the point of it: SQL order is now time order.
	rows, err := db.Reader().Query(`SELECT id FROM jobs WHERE id <> 'z' ORDER BY run_after`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var order string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		order += id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := "aebdc"; order != want {
		t.Errorf("ORDER BY run_after = %s, want %s (chronological)", order, want)
	}
}
