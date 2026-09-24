package worker

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

func openWorkerDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestWaitForSchemaAcceptsAFullyMigratedDatabase(t *testing.T) {
	db := openWorkerDB(t)
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := waitForSchema(t.Context(), db, slog.New(slog.DiscardHandler), time.Second); err != nil {
		t.Fatalf("waitForSchema on a migrated database: %v", err)
	}
}

// The regression: the guard was a hand-maintained "version >= 7" while the
// migrations had moved on to the fifties, so a database missing the newest
// migration passed it. Now the requirement is derived from what this binary
// embeds, so a new migration can never be one the guard does not know about.
func TestWaitForSchemaRefusesADatabaseMissingTheNewestMigration(t *testing.T) {
	db := openWorkerDB(t)
	ctx := t.Context()
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.MigrateDown(ctx, db); err != nil {
		t.Fatal(err)
	}
	known, err := sqlite.KnownSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}

	err = waitForSchema(ctx, db, slog.New(slog.DiscardHandler), 300*time.Millisecond)
	if err == nil {
		t.Fatal("a database one migration behind this binary was accepted")
	}
	if want := fmt.Sprintf("%05d", known); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the missing migration %s", err, want)
	}
}

// Version numbers cannot express this one: the database is at the highest
// known version and still missing a gap-filler (see sqlite.Migrate).
func TestWaitForSchemaRefusesADatabaseMissingAGapFiller(t *testing.T) {
	db := openWorkerDB(t)
	ctx := t.Context()
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer().ExecContext(ctx, `DELETE FROM goose_db_version WHERE version_id = 22`); err != nil {
		t.Fatal(err)
	}
	err := waitForSchema(ctx, db, slog.New(slog.DiscardHandler), 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "00022") {
		t.Fatalf("err = %v, want a refusal naming 00022", err)
	}
}

// The ordinary startup: roles start concurrently (ADR-0002) and the worker
// waits for the controller to finish migrating rather than failing.
func TestWaitForSchemaWaitsForTheControllerToMigrate(t *testing.T) {
	db := openWorkerDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- waitForSchema(ctx, db, slog.New(slog.DiscardHandler), 30*time.Second) }()

	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("waitForSchema after the controller migrated: %v", err)
	}
}

func TestDescribeVersions(t *testing.T) {
	cases := []struct {
		in   []int64
		want string
	}{
		{[]int64{22}, "00022"},
		{[]int64{1, 2, 3}, "00001, 00002, 00003"},
		{[]int64{41, 42, 43, 44, 45, 46, 54}, "00041, 00042, ... 00054 (7)"},
	}
	for _, tc := range cases {
		if got := describeVersions(tc.in); got != tc.want {
			t.Errorf("describeVersions(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
