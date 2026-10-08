package sqlite

import "testing"

// Migration 00059 (ADR-0104) adds service recipients: one live registration per
// key, a removed key free to be registered again, and a registration that goes
// with its executor principal.
func TestServiceRecipientsMigration(t *testing.T) {
	db := openUnmigrated(t)
	ctx := t.Context()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	const ts = "2026-01-01T00:00:00Z"
	exec := func(q string, args ...any) error {
		_, err := db.Writer().ExecContext(ctx, q, args...)
		return err
	}
	if err := exec(`INSERT INTO principals (id, kind, name, created_at) VALUES ('p-exe', 'executor', 'runner', ?)`, ts); err != nil {
		t.Fatal(err)
	}
	insert := func(id string) error {
		return exec(`INSERT INTO service_recipients (id, principal_id, recipient, fingerprint,
			registered_by_principal, registered_by_device, created_at) VALUES (?, 'p-exe', 'x25519:aa', 'FP', 'u', 'd', ?)`, id, ts)
	}
	if err := insert("r-1"); err != nil {
		t.Fatal(err)
	}
	if err := insert("r-2"); err == nil {
		t.Error("a second live registration of one key was admitted")
	}
	if err := exec(`UPDATE service_recipients SET revoked_at = ? WHERE id = 'r-1'`, ts); err != nil {
		t.Fatal(err)
	}
	if err := insert("r-2"); err != nil {
		t.Errorf("a removed key could not be registered again: %v", err)
	}
	if err := exec(`INSERT INTO service_recipients (id, principal_id, recipient, fingerprint,
		registered_by_principal, registered_by_device, created_at) VALUES ('r-3', 'nobody', 'x25519:bb', 'FP', 'u', 'd', ?)`, ts); err == nil {
		t.Error("a registration for a missing principal was admitted")
	}
	if err := exec(`DELETE FROM principals WHERE id = 'p-exe'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Reader().QueryRowContext(ctx, `SELECT count(*) FROM service_recipients`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d registrations outlived their principal", n)
	}
	migrateAllTheWayDown(t, db)
}
