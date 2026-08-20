package scanner_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/scanner"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// countingFS is the instrument the acceptance criterion is written against.
//
// "A rescan performs zero file reads" cannot be asserted by timing: a warm page
// cache makes a broken fingerprint cache look fast, and a slow CI runner makes
// a working one look broken. So every filesystem call the scanner makes goes
// through here and is counted, and the test asserts a number.
//
// Opens are counted separately from stats because they are the expensive half.
// A stat is metadata; an open is the door to reading 60 GB.
type countingFS struct {
	inner scanner.FS

	mu       sync.Mutex
	opens    int
	stats    int
	lstats   int
	readDirs int
	opened   []string

	// onOpen runs after each open, holding no lock. It is how a test cancels a
	// scan at a deterministic point rather than after a sleep.
	onOpen func(n int)
}

func newCountingFS() *countingFS { return &countingFS{inner: scanner.OSFS{}} }

func (c *countingFS) ReadDir(name string) ([]os.DirEntry, error) {
	c.mu.Lock()
	c.readDirs++
	c.mu.Unlock()
	return c.inner.ReadDir(name)
}

func (c *countingFS) Lstat(name string) (os.FileInfo, error) {
	c.mu.Lock()
	c.lstats++
	c.mu.Unlock()
	return c.inner.Lstat(name)
}

func (c *countingFS) Stat(name string) (os.FileInfo, error) {
	c.mu.Lock()
	c.stats++
	c.mu.Unlock()
	return c.inner.Stat(name)
}

func (c *countingFS) Open(name string) (io.ReadCloser, error) {
	c.mu.Lock()
	c.opens++
	n := c.opens
	c.opened = append(c.opened, name)
	hook := c.onOpen
	c.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return c.inner.Open(name)
}

func (c *countingFS) counts() (opens, stats, lstats, readDirs int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens, c.stats, c.lstats, c.readDirs
}

func (c *countingFS) Opens() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens
}

func (c *countingFS) openedPaths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.opened...)
}

func (c *countingFS) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opens, c.stats, c.lstats, c.readDirs = 0, 0, 0, 0
	c.opened = nil
}

var _ scanner.FS = (*countingFS)(nil)

// fixture is a real database, a real job queue, a real catalog and a real
// temporary filesystem. No mocking framework, per CLAUDE.md — the only thing
// standing in for something is the counter around the filesystem, and that is
// there to observe rather than to fake.
type fixture struct {
	t    *testing.T
	root string
	dir  string

	db    *sqlite.DB
	log   *events.Log
	cat   *catalog.Catalog
	queue *jobs.Queue
	fs    *countingFS

	libraryID string
	rootID    string
	casRoot   string

	scan *scanner.Scanner
}

type fixtureOptions struct {
	policy           scanner.Policy
	batchSize        int
	progressInterval int
}

func newFixture(t *testing.T, opts ...func(*fixtureOptions)) *fixture {
	t.Helper()

	data := t.TempDir()
	library := filepath.Join(data, "library")
	if err := os.MkdirAll(library, 0o750); err != nil {
		t.Fatalf("creating the library root: %v", err)
	}

	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(data, "heyarr.db")})
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatalf("opening the event log: %v", err)
	}
	cat, err := catalog.New(catalog.Options{DB: db, Events: eventLog, PeerName: "test"})
	if err != nil {
		t.Fatalf("opening the catalog: %v", err)
	}
	roots, err := cat.ReconcileLibraries(t.Context(), []catalog.LibrarySpec{{
		Name: "films", ContentType: "movie", Roots: []string{library},
	}})
	if err != nil {
		t.Fatalf("reconciling libraries: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("reconcile produced %d roots, want 1", len(roots))
	}

	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatalf("opening the job queue: %v", err)
	}

	cfg := fixtureOptions{batchSize: 64, progressInterval: 128}
	for _, apply := range opts {
		apply(&cfg)
	}

	fsys := newCountingFS()
	s, err := scanner.New(scanner.Options{
		FS:               fsys,
		Store:            cat,
		Queue:            queue,
		Policy:           cfg.policy,
		BatchSize:        cfg.batchSize,
		ProgressInterval: cfg.progressInterval,
	})
	if err != nil {
		t.Fatalf("building the scanner: %v", err)
	}

	return &fixture{
		t: t, root: data, dir: library,
		db: db, log: eventLog, cat: cat, queue: queue, fs: fsys,
		libraryID: roots[0].LibraryID, rootID: roots[0].ID,
		casRoot: filepath.Join(data, "cas"),
		scan:    s,
	}
}

// write creates a file under the library root, making parents as needed.
func (f *fixture) write(rel, content string) string {
	f.t.Helper()
	full := filepath.Join(f.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		f.t.Fatalf("creating %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		f.t.Fatalf("writing %s: %v", full, err)
	}
	return full
}

// writeTree lays down n files across a nested directory structure, so the walk
// is a walk rather than one ReadDir.
func (f *fixture) writeTree(n int) {
	f.t.Helper()
	for i := range n {
		rel := fmt.Sprintf("show-%02d/season-%02d/episode-%04d.mkv", i%10, (i/10)%10, i)
		f.write(rel, fmt.Sprintf("bytes for %d", i))
	}
}

func (f *fixture) mustScan() scanner.Progress {
	f.t.Helper()
	p, err := f.scan.Scan(f.t.Context(), scanner.Payload{RootID: f.rootID})
	if err != nil {
		f.t.Fatalf("scan: %v", err)
	}
	return p
}

// pendingIngests returns the ingest_artifact jobs waiting to run.
// killIngest drives one file's ingest job to dead, the way an ingest that keeps
// failing eventually would.
func (f *fixture) killIngest(relPath string) {
	f.t.Helper()
	res, err := f.db.Writer().ExecContext(f.t.Context(),
		`UPDATE jobs SET state = 'dead', lease_owner = NULL, lease_expires_at = NULL,
			last_error = 'killed by the test', finished_at = ?, updated_at = ?
		 WHERE type = ? AND dedupe_key = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano),
		ingest.JobType, ingest.DedupeKey(f.rootID, relPath))
	if err != nil {
		f.t.Fatalf("killing the ingest of %s: %v", relPath, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		f.t.Fatalf("killing the ingest of %s affected %d jobs, want 1", relPath, n)
	}
}

func (f *fixture) pendingIngests() []ingest.Payload {
	f.t.Helper()
	rows, err := f.db.Reader().QueryContext(f.t.Context(),
		`SELECT payload FROM jobs WHERE type = ? AND state = 'pending' ORDER BY id`, ingest.JobType)
	if err != nil {
		f.t.Fatalf("reading pending ingests: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ingest.Payload
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			f.t.Fatalf("reading pending ingests: %v", err)
		}
		var p ingest.Payload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			f.t.Fatalf("decoding an ingest payload: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("reading pending ingests: %v", err)
	}
	return out
}

// drainIngests does what ingest does to the filesystem — it opens and reads
// every enqueued file — and marks the jobs done.
//
// It goes through the SAME counting filesystem as the scan, which is what makes
// "the second pass read nothing" a measurement of the whole scan-to-ingest
// path rather than of the scanner in isolation. The scanner alone would trivially
// read nothing, because hashing is not its job.
func (f *fixture) drainIngests() int {
	f.t.Helper()
	rows, err := f.db.Reader().QueryContext(f.t.Context(),
		`SELECT id, payload FROM jobs WHERE type = ? AND state = 'pending'`, ingest.JobType)
	if err != nil {
		f.t.Fatalf("reading pending ingests: %v", err)
	}
	type job struct{ id, payload string }
	var pending []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.payload); err != nil {
			_ = rows.Close()
			f.t.Fatalf("reading pending ingests: %v", err)
		}
		pending = append(pending, j)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		f.t.Fatalf("reading pending ingests: %v", err)
	}
	_ = rows.Close()

	for _, j := range pending {
		var p ingest.Payload
		if err := json.Unmarshal([]byte(j.payload), &p); err != nil {
			f.t.Fatalf("decoding an ingest payload: %v", err)
		}
		file, err := f.fs.Open(p.Path)
		if err != nil {
			f.t.Fatalf("opening %s: %v", p.Path, err)
		}
		if _, err := io.Copy(io.Discard, file); err != nil {
			_ = file.Close()
			f.t.Fatalf("reading %s: %v", p.Path, err)
		}
		if err := file.Close(); err != nil {
			f.t.Fatalf("closing %s: %v", p.Path, err)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := f.db.Writer().ExecContext(f.t.Context(),
			`UPDATE jobs SET state = 'succeeded', finished_at = ?, updated_at = ? WHERE id = ?`,
			now, now, j.id); err != nil {
			f.t.Fatalf("completing job %s: %v", j.id, err)
		}
	}
	return len(pending)
}

// ingestPending runs the REAL ingest pipeline over the enqueued jobs, so that
// blobs, assets and replicas exist for the tests that assert on them.
func (f *fixture) ingestPending() {
	f.t.Helper()
	store, err := cas.OpenFS(f.casRoot)
	if err != nil {
		f.t.Fatalf("opening the CAS: %v", err)
	}
	pipeline, err := ingest.New(ingest.Options{
		Store:      newCASAdapter(store),
		Catalog:    f.cat,
		Identifier: identification.Default(),
	})
	if err != nil {
		f.t.Fatalf("building the ingest pipeline: %v", err)
	}
	for _, p := range f.pendingIngests() {
		if _, err := pipeline.Ingest(f.t.Context(), ingest.Request{
			RootID: p.RootID, SourcePath: p.Path, RelPath: p.RelPath, MIME: p.MIME,
		}); err != nil {
			f.t.Fatalf("ingesting %s: %v", p.Path, err)
		}
	}
	if _, err := f.db.Writer().ExecContext(f.t.Context(),
		`UPDATE jobs SET state = 'succeeded', finished_at = ?, updated_at = ? WHERE type = ? AND state = 'pending'`,
		time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano),
		ingest.JobType); err != nil {
		f.t.Fatalf("completing ingest jobs: %v", err)
	}
}

// casAdapter mirrors worker.CASByteStore. It is duplicated rather than imported
// because internal/worker imports this package's production code, and a test
// importing it back would be an import cycle.
type casAdapter struct{ store cas.Store }

func newCASAdapter(store cas.Store) *casAdapter { return &casAdapter{store: store} }

func (a *casAdapter) Link(ctx context.Context, sourcePath string, mode ingest.Materialisation) (ingest.Blob, error) {
	desc, err := a.store.Link(ctx, sourcePath, cas.Materialisation(mode))
	if err != nil {
		return ingest.Blob{}, err
	}
	return ingest.Blob{
		Hash: desc.Hash.String(), Size: desc.Size,
		Materialised: ingest.Materialisation(desc.Materialised),
		Deduplicated: desc.Deduplicated,
	}, nil
}

// count runs a scalar count query.
func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.Reader().QueryRowContext(f.t.Context(), query, args...).Scan(&n); err != nil {
		f.t.Fatalf("counting (%s): %v", query, err)
	}
	return n
}

func (f *fixture) scanRunState() (state string, p scanner.Progress) {
	f.t.Helper()
	err := f.db.Reader().QueryRowContext(f.t.Context(), `
		SELECT state, files_seen, files_enqueued, files_unchanged, files_skipped,
		       files_missing, errors, bytes_seen
		FROM scan_runs WHERE root_id = ? ORDER BY started_at DESC, id DESC LIMIT 1`, f.rootID).
		Scan(&state, &p.FilesSeen, &p.FilesEnqueued, &p.FilesUnchanged, &p.FilesSkipped,
			&p.FilesMissing, &p.Errors, &p.BytesSeen)
	if err != nil {
		f.t.Fatalf("reading the scan run: %v", err)
	}
	return state, p
}
