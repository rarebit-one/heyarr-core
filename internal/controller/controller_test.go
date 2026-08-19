package controller

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Database.Path = filepath.Join(dir, "heyarr.db")
	return cfg
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Regression from CI. Run receives the SHUTDOWN context, and it was being used
// for schema migration too — so a SIGTERM arriving while the migration was in
// flight cancelled it mid-statement and the role reported a startup failure.
// It passed on fast runners and failed on a slow macOS one, which is the worst
// way to find out.
//
// Transactional DDL means the interruption was safe rather than corrupting, but
// an ordinary restart should not surface as an error, and a service you are
// afraid to restart mid-upgrade is worse than one that takes a moment to stop.
func TestRunCompletesSchemaWorkEvenIfShutdownIsRequestedFirst(t *testing.T) {
	cfg := testConfig(t)

	// Already cancelled before Run is even entered — the most hostile version
	// of the race that broke CI.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := New(cfg, discard()).Run(ctx); err != nil {
		t.Fatalf("Run reported an error for what is an ordinary shutdown: %v", err)
	}

	// The schema work must still have happened, not been skipped.
	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		t.Fatalf("the database was never created: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	version, err := sqlite.SchemaVersion(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if version == 0 {
		t.Error("shutdown during startup left the database unmigrated")
	}
}

func TestRunReportsAnUnusableDatabase(t *testing.T) {
	cfg := testConfig(t)
	// A directory where the database file should be: openable, unusable.
	cfg.Database.Path = t.TempDir()

	err := New(cfg, discard()).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded against an unusable database path")
	}
	if !strings.Contains(err.Error(), "controller:") {
		t.Errorf("error = %q, want it to name the role that failed", err)
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- New(cfg, discard()).Run(ctx) }()

	// Let it get past startup, then ask it to stop.
	waitForMigratedDatabase(t, cfg.Database.Path)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on clean shutdown, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation")
	}
}

func waitForMigratedDatabase(t *testing.T, path string) {
	t.Helper()
	// Poll the file rather than opening the database. Opening in a tight loop
	// makes every attempt queue on busy_timeout, which starves the very
	// migration this is waiting for — the wait causes the thing it detects.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			time.Sleep(50 * time.Millisecond) // let the migration commit
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the controller never created its database")
}
