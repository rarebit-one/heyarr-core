package cli

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// The client commands are tested against a real API over a real database, with
// a real CAS behind the blob endpoints. Nothing is mocked and there is no fake
// transport: what is being tested is whether the CLI is a correct client of
// this API — pagination, problem documents, ranges, the event stream — and a
// fake server would only assert that the test's idea of the API is
// self-consistent.
//
// Everything that would otherwise differ per run is injected: the clock
// (ADR-0017), the identifier sequence, and the job queue's backoff jitter. That
// is what lets every --json shape be a golden file rather than a regex over the
// two fields that happen to be stable.

// testClock is the injected clock. It can be advanced, which is how a job is
// driven through five attempts and its backoff without the test taking a
// minute.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// harnessKnownSchemaVersion is the highest migration the harness pretends this
// binary embeds. It matches the applied version it reports, so the drift check
// on GET /api/v1/system is quiet by default and the --json goldens do not move
// every time a real migration lands.
const harnessKnownSchemaVersion = 4

// fixedTime is the instant everything created in these tests is stamped with.
var fixedTime = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

// idSequence hands out stable, sortable identifiers of the same shape as the
// real UUIDv7 ones.
type idSequence struct {
	mu sync.Mutex
	n  int
}

func (s *idSequence) next() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return "01990000-0000-7000-8000-00000000new" + string(rune('0'+s.n))
}

// apiHarness is a running Heyarr API the CLI can be pointed at.
type apiHarness struct {
	t          *testing.T
	db         *sqlite.DB
	server     *httptest.Server
	jobs       *jobs.Queue
	events     *events.Log
	tokens     *auth.Store
	cas        *cas.FS
	clock      *testClock
	configPath string
	dataDir    string
}

type harnessOptions struct {
	auth         bool
	streamBuffer int
}

type harnessOption func(*harnessOptions)

// withAPIAuth turns authentication on, which is what the credential-resolution
// tests need.
func withAPIAuth(o *harnessOptions) { o.auth = true }

func newAPIHarness(t *testing.T, opts ...harnessOption) *apiHarness {
	t.Helper()
	ctx := context.Background()

	var ho harnessOptions
	for _, o := range opts {
		o(&ho)
	}

	dir := t.TempDir()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	clock := &testClock{t: fixedTime}
	store, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobs.Options{
		Writer: db.Writer(), Reader: db.Reader(), Clock: clock, Events: eventLog,
		// Seeded, so a retried job's backoff is the same on every run and in
		// every order. The jitter is a production property, not a test one.
		Rand: rand.New(rand.NewPCG(1, 2)), // #nosec G404 -- determinism in a test
	})
	if err != nil {
		t.Fatal(err)
	}
	store2, err := cas.OpenFS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}

	api, err := resources.New(resources.Options{
		DB:              db,
		Jobs:            queue,
		Events:          eventLog,
		Tokens:          store,
		Logger:          slog.New(slog.DiscardHandler),
		Now:             clock.Now,
		NewID:           (&idSequence{}).next,
		StreamHeartbeat: 50 * time.Millisecond,
		StreamBuffer:    ho.streamBuffer,
	})
	if err != nil {
		t.Fatal(err)
	}
	blobHandler, err := blobs.New(blobs.Options{Store: store2, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	cfg.HTTP.Auth.Enabled = ho.auth

	srv, err := httpapi.New(httpapi.Options{
		Config:        cfg,
		Logger:        slog.New(slog.DiscardHandler),
		DB:            db,
		Verifier:      verifier,
		Events:        eventLog,
		Build:         buildinfo.Info{Version: "test", Commit: "abc123", Date: "2026-08-20T00:00:00Z"},
		SchemaVersion: 4,
		// Equal to SchemaVersion, so the golden --json shapes do not change
		// every time a migration is added. The drift tests set them apart
		// deliberately.
		KnownSchemaVersion: harnessKnownSchemaVersion,
		CASRoot:            store2.Root(),
		Mount:              []httpapi.MountFunc{api.Mount, blobHandler.Mount},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	configPath := filepath.Join(dir, "heyarr.yaml")
	body := "data_dir: " + dir + "\npeer:\n  name: test\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return &apiHarness{
		t: t, db: db, server: ts, jobs: queue, events: eventLog, tokens: store,
		cas: store2, clock: clock, configPath: configPath, dataDir: dir,
	}
}

// args prefixes a command line with the flags that point the CLI at this
// harness.
func (h *apiHarness) args(rest ...string) []string {
	return append([]string{"--config", h.configPath, "--addr", h.server.URL}, rest...)
}

// run executes a CLI command against the harness.
func (h *apiHarness) run(args ...string) (stdout, stderr string, err error) {
	h.t.Helper()
	return run(h.t, context.Background(), h.args(args...)...)
}

// mustRun executes a command and fails the test if it did not succeed.
func (h *apiHarness) mustRun(args ...string) string {
	h.t.Helper()
	out, errOut, err := h.run(args...)
	if err != nil {
		h.t.Fatalf("heyarr %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, out, errOut)
	}
	return out
}

func (h *apiHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Writer().ExecContext(context.Background(), query, args...); err != nil {
		h.t.Fatalf("seeding (%s): %v", query, err)
	}
}

// putBlob writes bytes into the CAS and records them in the catalog, which is
// what an ingest would have done.
func (h *apiHarness) putBlob(content string) cas.Descriptor {
	h.t.Helper()
	desc, err := h.cas.Put(context.Background(), strings.NewReader(content))
	if err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO blobs (hash, size, mime, chunked, first_seen_at)
		VALUES (?, ?, 'application/octet-stream', 0, ?)`,
		desc.Hash.String(), desc.Size, seedTime)
	return desc
}

// killJob drives a job to dead through the real queue: claim it, fail it,
// advance past the backoff, repeat until its attempts are exhausted.
//
// It is deliberately the queue's own state machine rather than an UPDATE that
// writes 'dead' into the row. A test that sets the state it wants to observe is
// testing its own SQL — and the transition it skips (attempts, finished_at,
// last_error) is exactly what the CLI reports.
func (h *apiHarness) killJob(id string) {
	h.t.Helper()
	ctx := context.Background()
	const owner = "test-worker"

	for i := 0; i < 20; i++ {
		job, err := h.jobs.Get(ctx, id)
		if err != nil {
			h.t.Fatalf("reading job %s: %v", id, err)
		}
		if job.State == jobs.Dead {
			return
		}
		// Past the longest backoff the queue can choose, so the job is due.
		h.clock.advance(30 * time.Minute)

		claimed, err := h.jobs.Claim(ctx, jobs.ClaimOptions{
			Owner: owner, Types: []string{job.Type}, LeaseTTL: time.Minute,
		})
		if errors.Is(err, jobs.ErrNoWork) {
			continue
		}
		if err != nil {
			h.t.Fatalf("claiming %s: %v", id, err)
		}
		if claimed.ID != id {
			h.t.Fatalf("claimed %s while trying to fail %s — the fixture has more than one claimable job",
				claimed.ID, id)
		}
		failure := errors.New("the library root is not readable")
		if err := h.jobs.Fail(ctx, claimed.ID, owner, failure); err != nil {
			h.t.Fatalf("failing %s: %v", id, err)
		}
	}
	h.t.Fatalf("job %s never reached dead", id)
}

// client builds an API client pointed at the harness, for the tests that
// exercise the client package's own logic rather than a command.
func (h *apiHarness) client(t *testing.T) *client.Client {
	t.Helper()
	c, err := client.New(client.Options{Addr: h.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// awaitScanJob polls until a scan_library job exists and returns its id.
//
// Polling for the condition rather than sleeping: the job is enqueued by
// another goroutine running the CLI, and a fixed wait is a bet on machine speed
// that CI eventually loses.
func (h *apiHarness) awaitScanJob() string {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var id string
		err := h.db.Reader().QueryRowContext(context.Background(),
			`SELECT id FROM jobs WHERE type = 'scan_library' ORDER BY id ASC LIMIT 1`).Scan(&id)
		if err == nil {
			return id
		}
		if !errors.Is(err, sql.ErrNoRows) {
			h.t.Fatalf("looking for the scan job: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatal("no scan job was ever enqueued")
	return ""
}

// serveUnix binds the same handler on a unix socket and returns its path, so
// the default transport is exercised end to end rather than assumed to work.
func (h *apiHarness) serveUnix(t *testing.T) string {
	t.Helper()
	// Not t.TempDir(): a socket path is a fixed-size array in a C struct (104
	// bytes on darwin), and the framework's temporary paths are long enough to
	// exceed it — which presents as "bind: invalid argument".
	dir, err := os.MkdirTemp("", "hy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets are not usable here: %v", err)
	}
	srv := &http.Server{Handler: h.server.Config.Handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socket
}

// corruptBlobFile rewrites a blob's bytes underneath the CAS, which is what a
// failing disk or an external tool editing a hard-linked original does.
func corruptBlobFile(t *testing.T, h *apiHarness, hash string) {
	t.Helper()
	found := ""
	err := filepath.WalkDir(h.cas.Root(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(hash, d.Name()) {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatalf("no file in the CAS is named after %s", hash)
	}
	// The stored file is read-only, as the CAS intends.
	if err := os.Chmod(found, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(found, []byte("not those bytes at all"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// normalisePaths replaces the harness's temporary directory, which is the one
// thing in a --json shape that legitimately differs per run.
func (h *apiHarness) normalisePaths(s string) string {
	return strings.ReplaceAll(s, h.dataDir, "<data_dir>")
}

// ---------------------------------------------------------------------------
// Fixtures — every identifier and timestamp fixed, so the golden files change
// only when a shape does.
// ---------------------------------------------------------------------------

const (
	peerID     = "01990000-0000-7000-8000-000000000p01"
	libFilmsID = "01990000-0000-7000-8000-0000000000l1"
	libBooksID = "01990000-0000-7000-8000-0000000000l2"
	rootID     = "01990000-0000-7000-8000-0000000000r1"
	work1ID    = "01990000-0000-7000-8000-0000000000w1"
	work2ID    = "01990000-0000-7000-8000-0000000000w2"
	work3ID    = "01990000-0000-7000-8000-0000000000w3"
	edition1ID = "01990000-0000-7000-8000-0000000000e1"
	edition2ID = "01990000-0000-7000-8000-0000000000e2"
	edition3ID = "01990000-0000-7000-8000-0000000000e3"
	asset1ID   = "01990000-0000-7000-8000-0000000000a1"
	asset2ID   = "01990000-0000-7000-8000-0000000000a2"
	asset3ID   = "01990000-0000-7000-8000-0000000000a3"
	jobDeadID  = "01990000-0000-7000-8000-0000000000j2"

	blob1Hash = "blake3:1111111111111111111111111111111111111111111111111111111111111111"
	blob2Hash = "blake3:2222222222222222222222222222222222222222222222222222222222222222"

	seedTime = "2026-08-01T00:00:00Z"
)

// seed writes a small catalog: two libraries, three works, three assets, one
// dead job and one peer.
func (h *apiHarness) seed() *apiHarness {
	h.t.Helper()

	h.exec(`INSERT INTO peers (id, name, site, mode, endpoint, is_self, created_at)
		VALUES (?, 'peer-a', 'site-a', 'full', 'http://127.0.0.1:7777', 1, ?)`, peerID, seedTime)

	h.exec(`INSERT INTO libraries (id, name, content_type, enabled, created_at) VALUES
		(?, 'films', 'movie', 1, ?), (?, 'books', 'book', 1, ?)`,
		libFilmsID, seedTime, libBooksID, seedTime)
	h.exec(`INSERT INTO library_roots (id, library_id, path, ingest_mode, enabled, created_at)
		VALUES (?, ?, '/srv/films', 'reflink', 1, ?)`, rootID, libFilmsID, seedTime)

	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at) VALUES
		(?, 'movie', 'arrival|2016', 'Arrival', 'arrival', 2016, '{"director":"Villeneuve"}', ?, ?),
		(?, 'movie', 'blade-runner-2049|2017', 'Blade Runner 2049', 'blade runner 2049', 2017, '{}', ?, ?),
		(?, 'book', 'dune|1965', 'Dune', 'dune', 1965, '{}', ?, ?)`,
		work1ID, seedTime, seedTime, work2ID, seedTime, seedTime, work3ID, seedTime, seedTime)

	h.exec(`INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at) VALUES
		(?, ?, '2160p', 'remux', 'en', '{"hdr":"dv"}', ?),
		(?, ?, '1080p', 'web', 'en', '{}', ?),
		(?, ?, 'first', 'print', NULL, '{}', ?)`,
		edition1ID, work1ID, seedTime, edition2ID, work2ID, seedTime, edition3ID, work3ID, seedTime)

	h.exec(`INSERT INTO blobs (hash, size, mime, chunked, first_seen_at) VALUES
		(?, 42949672960, 'video/x-matroska', 0, ?), (?, 8589934592, 'video/mp4', 0, ?)`,
		blob1Hash, seedTime, blob2Hash, seedTime)

	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at) VALUES
		(?, ?, ?, 'managed', ?, '/srv/films/Arrival.mkv', 'primary', 'Arrival.mkv', 'video/x-matroska', 'path', NULL, ?, ?),
		(?, ?, ?, 'managed', ?, '/srv/films/BR2049.mp4', 'primary', 'BR2049.mp4', 'video/mp4', 'path', ?, ?, ?),
		(?, ?, ?, 'linked', NULL, '/srv/books/Dune.epub', 'primary', 'Dune.epub', 'application/epub+zip', 'filename', NULL, ?, ?)`,
		asset1ID, edition1ID, libFilmsID, blob1Hash, seedTime, seedTime,
		asset2ID, edition2ID, libFilmsID, blob2Hash, seedTime, seedTime, seedTime,
		asset3ID, edition3ID, libBooksID, seedTime, seedTime)

	h.exec(`INSERT INTO jobs (id, type, payload, state, priority, dedupe_key, required_capability,
			run_after, attempts, max_attempts, last_error, created_at, updated_at, finished_at) VALUES
		(?, 'ingest_artifact', '{"path":"/srv/films/Broken.mkv"}', 'dead', 100, NULL, '',
			?, 5, 5, 'the file could not be read', ?, ?, ?)`,
		jobDeadID, seedTime, seedTime, seedTime, seedTime)

	return h
}
