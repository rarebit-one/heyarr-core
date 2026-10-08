package sqlite

import "testing"

// Migration 00058 (ADR-0104) rebuilds principals to admit the executor kind,
// and adds a restricted flag to every token and an owner to every space. The
// databases that matter are the ones with rows in them: an existing service
// principal, its token, a user and a space must come through unchanged — the
// token unrestricted, the space ownerless (a legacy household space) — and the
// foreign keys that point at principals must still bite after the rebuild.
func TestSpaceAccessMigrationKeepsExistingPrincipals(t *testing.T) {
	db := openUnmigrated(t)
	ctx := t.Context()
	migrateTo(t, db, 57)

	const ts = "2026-01-01T00:00:00Z"
	for _, q := range []string{
		`INSERT INTO principals (id, kind, name, created_at) VALUES ('p-svc', 'service', 'player', '` + ts + `')`,
		`INSERT INTO principals (id, kind, name, created_at) VALUES ('p-usr', 'user', 'someone', '` + ts + `')`,
		`INSERT INTO api_tokens (id, principal_id, name, token_hash, scopes, created_at)
		 VALUES ('t-1', 'p-svc', 'player', 'hash', 'read,write', '` + ts + `')`,
		`INSERT INTO encrypted_spaces (id, kind, created_at) VALUES ('s-1', 'family', '` + ts + `')`,
	} {
		if _, err := db.Writer().ExecContext(ctx, q); err != nil {
			t.Fatalf("seeding a pre-00058 database: %v", err)
		}
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("upgrading through 00058: %v", err)
	}

	var kind string
	var restricted int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT p.kind, t.restricted FROM api_tokens t JOIN principals p ON p.id = t.principal_id WHERE t.id = 't-1'`).
		Scan(&kind, &restricted); err != nil {
		t.Fatalf("the existing token did not survive the upgrade: %v", err)
	}
	if kind != "service" || restricted != 0 {
		t.Errorf("existing token came back as kind=%q restricted=%d, want service / 0", kind, restricted)
	}
	var owner *string
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT owner_principal_id FROM encrypted_spaces WHERE id = 's-1'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != nil {
		t.Errorf("an existing space gained owner %q; it must stay a legacy (NULL-owner) space", *owner)
	}

	// The new kind is admitted, and an unknown one still is not.
	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO principals (id, kind, name, created_at) VALUES ('p-exe', 'executor', 'runner', ?)`, ts); err != nil {
		t.Fatalf("an executor principal was refused: %v", err)
	}
	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO principals (id, kind, name, created_at) VALUES ('p-bad', 'robot', 'x', ?)`, ts); err == nil {
		t.Error("an unknown principal kind was admitted")
	}

	// The rebuilt table is still what api_tokens references: a token for a
	// principal that does not exist is refused, and deleting a principal still
	// cascades to its tokens.
	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO api_tokens (id, principal_id, name, token_hash, scopes, created_at)
		 VALUES ('t-x', 'nobody', 'x', 'h', 'read', ?)`, ts); err == nil {
		t.Error("a token for a missing principal was admitted: the rebuild lost the foreign key")
	}
	if _, err := db.Writer().ExecContext(ctx, `DELETE FROM principals WHERE id = 'p-svc'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Reader().QueryRowContext(ctx, `SELECT count(*) FROM api_tokens WHERE id = 't-1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("deleting a principal no longer cascades to its tokens")
	}

	var integrity string
	if err := db.Reader().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Errorf("integrity_check = %q after the upgrade", integrity)
	}
	migrateAllTheWayDown(t, db)
}
