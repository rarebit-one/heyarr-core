package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/scanner"
)

// The libraries block did nothing at all until M1-12: it parsed, validated and
// was then ignored. These tests are what stop it going back to that.
func TestTheControllerReconcilesLibrariesAndSchedulesAScan(t *testing.T) {
	cfg := testConfig(t)
	films := filepath.Join(t.TempDir(), "films")
	if err := os.MkdirAll(films, 0o750); err != nil {
		t.Fatalf("creating %s: %v", films, err)
	}
	cfg.Libraries = []config.Library{{Name: "films", ContentType: "movie", Roots: []string{films}}}

	// An already-cancelled context: reconciliation is startup work and must
	// complete, exactly like the migration it follows.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(cfg, discard()).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	db := openDB(t, cfg)
	if got := count(t, db, `SELECT count(*) FROM libraries WHERE name = 'films'`); got != 1 {
		t.Fatalf("%d rows in libraries, want 1", got)
	}
	if got := count(t, db, `SELECT count(*) FROM library_roots WHERE path = ?`, films); got != 1 {
		t.Fatalf("%d rows in library_roots for %s, want 1", got, films)
	}
	if got := count(t, db, `SELECT count(*) FROM jobs WHERE type = ?`, scanner.JobType); got != 1 {
		t.Fatalf("%d scan_library jobs after one start, want 1 — nothing would ever scan the root", got)
	}
	_ = db.Close()

	// A second start must not duplicate the rows, and the pending scan from the
	// first start must not become two (ADR-0008).
	if err := New(cfg, discard()).Run(ctx); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	db = openDB(t, cfg)
	defer func() { _ = db.Close() }()
	if got := count(t, db, `SELECT count(*) FROM libraries`); got != 1 {
		t.Fatalf("%d libraries after two starts, want 1", got)
	}
	if got := count(t, db, `SELECT count(*) FROM library_roots`); got != 1 {
		t.Fatalf("%d library roots after two starts, want 1", got)
	}
	if got := count(t, db, `SELECT count(*) FROM jobs WHERE type = ?`, scanner.JobType); got != 1 {
		t.Fatalf("%d scan_library jobs after two starts, want 1 — the dedupe key is not holding", got)
	}
}

// Changing a library's content type in place would silently re-identify every
// work under it. Refusing at startup is the difference between an operator
// seeing a message and an operator seeing a rebuilt catalog.
func TestTheControllerRefusesAChangedContentType(t *testing.T) {
	cfg := testConfig(t)
	dir := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	cfg.Libraries = []config.Library{{Name: "mixed", ContentType: "movie", Roots: []string{dir}}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(cfg, discard()).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cfg.Libraries[0].ContentType = "series"
	err := New(cfg, discard()).Run(ctx)
	if err == nil {
		t.Fatal("the controller accepted a library whose content type changed under it")
	}
	if !strings.Contains(err.Error(), "content type cannot be changed") {
		t.Fatalf("error = %v, want it to say the content type cannot be changed", err)
	}
}

func openDB(t *testing.T, cfg config.Config) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	return db
}

func count(t *testing.T, db *sqlite.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Reader().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("counting (%s): %v", query, err)
	}
	return n
}
