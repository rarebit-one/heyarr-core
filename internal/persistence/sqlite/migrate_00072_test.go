package sqlite

import (
	"database/sql"
	"strings"
	"testing"
)

// TestMigration72KeepsEveryExistingPinPermanent: provisional upload pins
// (ADR-0096 amendment, #714) are opt-in per upload, so a pin recorded before
// 00072 must come through as permanent — a NULL expiry — and never become
// eligible for expiry by being migrated.
func TestMigration72KeepsEveryExistingPinPermanent(t *testing.T) {
	db := openUnmigrated(t)
	migrateTo(t, db, 71)
	blob := "blake3:" + strings.Repeat("a", 64)
	if _, err := db.Writer().ExecContext(t.Context(),
		`INSERT INTO placement_pins (blob_hash, peer_id, created_at) VALUES (?, 'peer-a', '2026-10-01T00:00:00Z')`,
		blob); err != nil {
		t.Fatal(err)
	}
	migrateTo(t, db, 72)

	var until, uploader sql.NullString
	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT provisional_until, uploader FROM placement_pins WHERE blob_hash = ?`, blob).Scan(&until, &uploader); err != nil {
		t.Fatal(err)
	}
	if until.Valid || uploader.Valid {
		t.Errorf("a pre-00072 pin came through as (provisional_until=%v, uploader=%v), want both NULL — permanent",
			until, uploader)
	}
}
