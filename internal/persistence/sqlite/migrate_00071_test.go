package sqlite

import (
	"encoding/json"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// The rom-indexed row as an installation seeded it before ADR-0121 §5.
const (
	romIndexedV70Description = "Starting profile for searched ROMs. Accepts a nonempty release " +
		"for the wanted platform and stops once held; region and revision are not judged yet."
	romIndexedV70Rules = `[{"attribute":"size_bytes","op":"gte","value":1}]`
)

func insertROMIndexed(t *testing.T, db *DB, id, contentTypes, prefer string) {
	t.Helper()
	if _, err := db.Writer().ExecContext(t.Context(), `
		INSERT INTO quality_profiles
			(id, name, description, accept, prefer, terminal, content_types, seeded, created_at, updated_at)
		VALUES (?, 'rom-indexed', ?, ?, ?, ?, ?, 1, '2026-10-21T00:00:00Z', '2026-10-21T00:00:00Z')`,
		id, romIndexedV70Description, romIndexedV70Rules, prefer, romIndexedV70Rules, contentTypes); err != nil {
		t.Fatal(err)
	}
}

func profileRow(t *testing.T, db *DB, id string) (description, prefer string) {
	t.Helper()
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT description, prefer FROM quality_profiles WHERE id = ?`, id).Scan(&description, &prefer); err != nil {
		t.Fatal(err)
	}
	return description, prefer
}

// TestMigration71GivesAnUntouchedROMProfileItsRegionPreference: seeding never
// updates an existing profile, so 00071 is what brings an installation's
// untouched rom-indexed to the current default, byte for byte. A row an
// operator edited keeps their edit, and the rollback restores the old default.
func TestMigration71GivesAnUntouchedROMProfileItsRegionPreference(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 70)
	insertROMIndexed(t, db, "seeded", `["rom"]`, "[]")
	migrateTo(t, db, 71)

	var want policy.Profile
	for _, p := range policy.Defaults() {
		if p.Name == "rom-indexed" {
			want = p
		}
	}
	wantPrefer, err := json.Marshal(want.Prefer)
	if err != nil {
		t.Fatal(err)
	}
	description, prefer := profileRow(t, db, "seeded")
	if prefer != string(wantPrefer) || description != want.Description {
		t.Fatalf("after 00071 the seeded row is (%q, %s), want the current default (%q, %s)",
			description, prefer, want.Description, wantPrefer)
	}

	if err := MigrateDown(t.Context(), db); err != nil {
		t.Fatalf("MigrateDown from 71: %v", err)
	}
	if description, prefer = profileRow(t, db, "seeded"); prefer != "[]" || description != romIndexedV70Description {
		t.Fatalf("after rolling 00071 back the row is (%q, %s), want the old default", description, prefer)
	}
}

// An API edit keeps seeded = 1, so a row changed in ANY column (its rules, or
// only the content types it is offered for) is the operator's and is left alone.
func TestMigration71LeavesAnEditedROMProfileAlone(t *testing.T) {
	for _, tc := range []struct{ name, contentTypes, prefer string }{
		{"rules", `["rom"]`, `[{"attribute":"region","op":"eq","value":"japan","weight":5}]`},
		{"content types", `["rom","game"]`, "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openUnmigrated(t)
			migrateTo(t, db, 70)
			insertROMIndexed(t, db, "edited", tc.contentTypes, tc.prefer)
			migrateTo(t, db, 71)
			if description, prefer := profileRow(t, db, "edited"); prefer != tc.prefer || description != romIndexedV70Description {
				t.Fatalf("00071 rewrote an operator's rom-indexed: now (%q, %s)", description, prefer)
			}
		})
	}
}

// The rollback is guarded the same way: a row an operator edited after the
// upgrade, even in a column 00071 never wrote, keeps its preferences.
func TestMigration71RollbackLeavesAnEditedROMProfileAlone(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 70)
	insertROMIndexed(t, db, "edited", `["rom"]`, "[]")
	migrateTo(t, db, 71)
	_, upgraded := profileRow(t, db, "edited")
	if _, err := db.Writer().ExecContext(t.Context(),
		`UPDATE quality_profiles SET accept = '[{"attribute":"size_bytes","op":"gte","value":1048576}]' WHERE id = 'edited'`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateDown(t.Context(), db); err != nil {
		t.Fatalf("MigrateDown from 71: %v", err)
	}
	if _, prefer := profileRow(t, db, "edited"); prefer != upgraded {
		t.Fatalf("rolling 00071 back reset an edited profile's preferences to %s", prefer)
	}
}
