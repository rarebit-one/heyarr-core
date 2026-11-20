package controller

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/scanner"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
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

// A library row whose type predates the gates (#896) is named at start, with or
// without a libraries block — an API-created library has the same history —
// and a correct one is not.
func TestTheControllerWarnsAboutALibraryWithAnUnknownContentType(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(cfg, discard()).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	db := openDB(t, cfg)
	for _, q := range []string{
		`INSERT INTO libraries (id, name, content_type, enabled, created_at)
			VALUES ('lib-tv', 'tv', 'show', 1, '2026-08-01T00:00:00Z')`,
		`INSERT INTO libraries (id, name, content_type, enabled, created_at)
			VALUES ('lib-films', 'films', 'movie', 1, '2026-08-01T00:00:00Z')`,
	} {
		if _, err := db.Writer().ExecContext(context.Background(), q); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	_ = db.Close()

	var buf bytes.Buffer
	if err := New(cfg, slog.New(slog.NewTextHandler(&buf, nil))).Run(ctx); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	var warned []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "content type Heyarr does not understand") {
			warned = append(warned, line)
		}
	}
	if len(warned) != 1 {
		t.Fatalf("%d warnings, want exactly one (for tv):\n%s", len(warned), buf.String())
	}
	for _, want := range []string{"level=WARN", "library=tv", "content_type=show", "series", "heyarr library retype tv <type>"} {
		if !strings.Contains(warned[0], want) {
			t.Errorf("warning does not contain %q:\n%s", want, warned[0])
		}
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

// The startup guard, at the level an operator meets it.
//
// The correct layout must be SILENT — a warning that fires on a healthy host
// is one everybody learns to ignore, and then the real one is ignored too.
func TestTheIngestGuardIsSilentWhenTheStoreAndTheLibraryCanLink(t *testing.T) {
	base := t.TempDir()
	library := filepath.Join(base, "media")
	casRoot := filepath.Join(base, "media", "heyarr", "cas")
	for _, d := range []string{library, filepath.Join(casRoot, "tmp")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(library, "film.mkv"), []byte("bytes"), 0o600); err != nil {
		t.Fatalf("writing a library file: %v", err)
	}

	var buf bytes.Buffer
	warnIfIngestWillCopy(casRoot, library, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	if strings.Contains(buf.String(), "COPY every file") {
		t.Errorf("warned about a library the store can hardlink from:\n%s", buf.String())
	}
}

// And when it cannot link, the warning must fire and say what it is evidence
// OF. #222's guard was silent on the one host where the problem was real, so
// "it warns" is not enough on its own: an operator needs to know whether they
// are reading a measurement or a prediction.
func TestTheIngestGuardWarnsAndSaysWhichInstrumentSawIt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("no second filesystem that is reliably present off Linux")
	}
	casRoot := t.TempDir()
	library, err := os.MkdirTemp("/dev/shm", "heyarr-library-*")
	if err != nil {
		t.Skipf("cannot create a library on a second filesystem (/dev/shm): %v", err)
	}
	defer func() { _ = os.RemoveAll(library) }()
	if same, known, err := cas.SameFilesystem(casRoot, library); err != nil || !known || same {
		t.Skipf("the temp dir and /dev/shm are one filesystem here (same=%v known=%v err=%v)", same, known, err)
	}
	if err := os.WriteFile(filepath.Join(library, "film.mkv"), []byte("bytes"), 0o600); err != nil {
		t.Fatalf("writing a library file: %v", err)
	}

	var buf bytes.Buffer
	warnIfIngestWillCopy(casRoot, library, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	got := buf.String()
	for _, want := range []string{
		"level=WARN",
		"COPY every file",
		cas.InstrumentProbe,
		// The kernel's own words, carried through to the operator.
		"cross-device",
		// And the consequence, because "different mounts" means nothing to
		// somebody who has not read ADR-0014.
		"second full copy",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not contain %q:\n%s", want, got)
		}
	}
}
