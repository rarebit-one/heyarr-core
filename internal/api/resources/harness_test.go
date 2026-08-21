// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// fixedClock is the injected clock (ADR-0017). Nothing in these tests reads
// wall time, which is what lets every response shape be a golden file rather
// than a set of regexes over the fields that happen to be stable.
type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

// fixedTime is the instant every created resource is stamped with.
var fixedTime = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

// harness is a running API over a real database. Nothing is mocked: these
// tests are about the interaction between chi, the middleware chain, the
// queries and SQLite, and a mock would assert that the test's idea of that
// interaction is self-consistent.
type harness struct {
	t      *testing.T
	db     *sqlite.DB
	server *httpapi.Server
	http   *httptest.Server
	store  *auth.Store
	jobs   *jobs.Queue
	events *events.Log
	clock  *fixedClock
	// ids hands out deterministic identifiers for created resources.
	ids *idSequence
}

// idSequence produces stable, sortable identifiers so that a POST response can
// be a golden file. Real ones are UUIDv7 (ADR-0017); these are the same shape.
type idSequence struct{ n int }

func (s *idSequence) next() string {
	s.n++
	return "01990000-0000-7000-8000-00000000new" + string(rune('0'+s.n))
}

// harnessConfig is what a test may vary about the server under test.
type harnessConfig struct {
	cfg          config.Config
	streamBuffer int
	// providers is the registry the API reports on. Nil means "this node has
	// none configured", which is the supported degrade path (ADR-0025) and the
	// default for every test that is not about providers.
	providers *providers.Registry
}

// withProviders gives the harness a provider registry, for the tests that are
// about what a configured node reports.
func withProviders(reg *providers.Registry) harnessOption {
	return func(hc *harnessConfig) { hc.providers = reg }
}

type harnessOption func(*harnessConfig)

// withAuth turns authentication on, which is what the scope tests need. Most
// tests run with it off: they are testing resources, and every one of them
// carrying a token would test the token plumbing many times and the resources
// once.
func withAuth(hc *harnessConfig) { hc.cfg.HTTP.Auth.Enabled = true }

// withStreamBuffer shrinks the SSE subscription buffer, so that the drop path
// can be driven deliberately rather than by flooding a socket and hoping.
func withStreamBuffer(n int) harnessOption {
	return func(hc *harnessConfig) { hc.streamBuffer = n }
}

func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	hc := harnessConfig{cfg: config.Defaults()}
	hc.cfg.DataDir = dir
	hc.cfg.HTTP.Auth.Enabled = false
	hc.cfg.HTTP.Addr = "127.0.0.1:0"
	hc.cfg.HTTP.UnixSocket = ""
	for _, o := range opts {
		o(&hc)
	}
	cfg := hc.cfg

	clock := &fixedClock{t: fixedTime}
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
	})
	if err != nil {
		t.Fatal(err)
	}

	ids := &idSequence{}
	// The catalog answers §56's satisfaction questions and §60's upgrade
	// question. Without it the satisfaction routes are not mounted at all —
	// which is how they went untested at this layer when M3-05 added them.
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: "test", PeerSite: "test-site", Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}

	api, err := resources.New(resources.Options{
		DB:        db,
		Jobs:      queue,
		Events:    eventLog,
		Tokens:    store,
		Catalog:   cat,
		Providers: hc.providers,
		Logger:    slog.New(slog.DiscardHandler),
		Now:       clock.Now,
		NewID:     ids.next,
		// Short enough that an idle stream heartbeats within a test's patience,
		// long enough that it is not the thing under test.
		StreamHeartbeat: 50 * time.Millisecond,
		StreamBuffer:    hc.streamBuffer,
	})
	if err != nil {
		t.Fatal(err)
	}

	srv, err := httpapi.New(httpapi.Options{
		Config:        cfg,
		Logger:        slog.New(slog.DiscardHandler),
		DB:            db,
		Verifier:      verifier,
		Events:        eventLog,
		Build:         buildinfo.Info{Version: "test", Commit: "abc123", Date: "2026-08-20T00:00:00Z"},
		SchemaVersion: 4,
		Mount:         []httpapi.MountFunc{api.Mount},
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &harness{
		t: t, db: db, server: srv, http: ts, store: store,
		jobs: queue, events: eventLog, clock: clock, ids: ids,
	}
}

// do issues a request against the real handler.
func (h *harness) do(method, path, token string, body io.Reader) *http.Response {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, h.http.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (h *harness) get(path string) *http.Response { return h.do(http.MethodGet, path, "", nil) }

// goldenRequestID is sent as an inbound correlation id, which the server
// honours when it is short and printable. Without it every problem document
// would carry a fresh UUID and could not be a golden file at all.
const goldenRequestID = "golden-request-id"

func (h *harness) doStable(method, path string, body io.Reader) *http.Response {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, h.http.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(httpapi.RequestIDHeader, goldenRequestID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (h *harness) body(resp *http.Response) []byte {
	h.t.Helper()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return buf
}

// indent renders a response body as stable, readable JSON for a golden file. A
// golden file of minified JSON is a golden file nobody reviews.
//
// It reformats the bytes rather than decoding and re-encoding them, because a
// round trip through map[string]any turns every number into a float64 — and a
// 40 GB blob's size then appears in the golden file as 4.294967296e+10, which
// is not what the API returned.
func (h *harness) indent(resp *http.Response) []byte {
	h.t.Helper()
	raw := h.body(resp)
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		h.t.Fatalf("the response is not JSON: %s", raw)
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

// redact replaces the named keys, wherever they appear, with a placeholder.
//
// It exists for exactly two responses: a minted token and a queued job, whose
// identifiers are generated inside packages this test does not own. Everything
// else is golden-tested byte for byte, and redacting more than the two fields
// that genuinely cannot be fixed would quietly stop the golden files from
// testing anything.
func (h *harness) redact(resp *http.Response, keys ...string) []byte {
	h.t.Helper()
	var doc any
	if err := json.Unmarshal(h.body(resp), &doc); err != nil {
		h.t.Fatalf("the response is not JSON: %v", err)
	}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	var walk func(any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case map[string]any:
			for k, inner := range t {
				if want[k] {
					t[k] = "<redacted>"
					continue
				}
				t[k] = walk(inner)
			}
			return t
		case []any:
			for i, inner := range t {
				t[i] = walk(inner)
			}
			return t
		default:
			return v
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(walk(doc)); err != nil {
		h.t.Fatal(err)
	}
	return buf.Bytes()
}

func (h *harness) mint(name string, scopes ...auth.Scope) auth.CreatedToken {
	h.t.Helper()
	created, err := h.store.Create(context.Background(), name, scopes, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return created
}

// countRows asserts against the database directly.
//
// It exists because asserting a count through the API is only sound when the
// API supports the filter being used: an unsupported query parameter is
// ignored rather than refused, so the assertion silently becomes a count of
// everything. That happened once while this file was being written and the
// test passed at three.
func (h *harness) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Writer().ExecContext(context.Background(), query, args...); err != nil {
		h.t.Fatalf("seeding (%s): %v", query, err)
	}
}

// eventsOfType counts the events of the given types.
//
// Counting EVERY event makes an assertion about one feature sensitive to every
// other feature that happens to emit — creating a want now also enqueues a
// reconciliation, and a raw count would move with a change that has nothing to
// do with what the test is checking. Naming the types says what the test means.
func (h *harness) eventsOfType(t *testing.T, types ...string) int {
	t.Helper()
	evs, err := h.events.Since(context.Background(), 0, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, ty := range types {
		want[ty] = true
	}
	var n int
	for _, e := range evs {
		if want[e.Type] {
			n++
		}
	}
	return n
}

// execErr runs a statement that is EXPECTED to be refused, returning the
// error rather than failing the test. exec is for seeding; this is for
// asserting that the database says no.
func (h *harness) execErr(query string, args ...any) error {
	h.t.Helper()
	_, err := h.db.Writer().ExecContext(context.Background(), query, args...)
	return err
}

func decodeProblem(t *testing.T, resp *http.Response, raw []byte) problem.Problem {
	t.Helper()
	if got := resp.Header.Get("Content-Type"); got != problem.MediaType {
		t.Errorf("Content-Type = %q, want %q", got, problem.MediaType)
	}
	var p problem.Problem
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("the error body is not a problem document: %s", raw)
	}
	return p
}

// ---------------------------------------------------------------------------
// Fixtures
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
	job1ID     = "01990000-0000-7000-8000-0000000000j1"
	job2ID     = "01990000-0000-7000-8000-0000000000j2"
	device1ID  = "01990000-0000-7000-8000-0000000000d1"
	device2ID  = "01990000-0000-7000-8000-0000000000d2"
	session1ID = "01990000-0000-7000-8000-0000000000s1"
	session2ID = "01990000-0000-7000-8000-0000000000s2"

	// Two quality profiles spanning the distinction §62 turns on: one that can
	// be finished, and one that never can. A seed set where every profile
	// terminates would leave the "never stop looking" path unexercised by
	// anything the HTTP layer sees.
	profile1ID = "01990000-0000-7000-8000-0000000000q1"
	profile2ID = "01990000-0000-7000-8000-0000000000q2"

	// Two wants over ONE work under DIFFERENT profiles — the §61 rule the
	// schema exists to permit, present in the fixture so the list shape shows
	// it rather than only a unit test asserting it.
	desired1ID = "01990000-0000-7000-8000-0000000000i1"
	desired2ID = "01990000-0000-7000-8000-0000000000i2"

	blob1Hash = "blake3:1111111111111111111111111111111111111111111111111111111111111111"
	blob2Hash = "blake3:2222222222222222222222222222222222222222222222222222222222222222"

	seedTime = "2026-08-01T00:00:00Z"
)

// seed writes a small, fully deterministic catalog. Every identifier and
// timestamp is fixed, so the golden files change only when the shape does.
func (h *harness) seed() *harness {
	h.t.Helper()

	h.exec(`INSERT INTO peers (id, name, site, mode, endpoint, is_self, created_at)
		VALUES (?, 'peer-a', 'site-a', 'full', 'http://127.0.0.1:8385', 1, ?)`, peerID, seedTime)

	// Two devices spanning what the planner will have to distinguish: one that
	// takes almost everything, and one deliberately limited so that the
	// non-DIRECT path has something real to be tested against in M2-07.
	h.exec(`INSERT INTO devices
		(id, device_key, name, platform, max_width, max_height, max_bitrate_bps, supports_hdr,
		 containers, video_codecs, audio_codecs, created_at, updated_at, last_seen_at) VALUES
		(?, 'tv-living-room', 'Living Room', 'tvos', 3840, 2160, 120000000, 1,
		 '["mp4","mkv"]', '["h264","hevc"]', '["aac","eac3"]', ?, ?, ?),
		(?, 'kitchen-speaker', 'Kitchen Speaker', 'linux', 0, 0, 320000, 0,
		 '["mp3","flac"]', '[]', '["mp3","flac"]', ?, ?, ?)`,
		device1ID, seedTime, seedTime, seedTime,
		device2ID, seedTime, seedTime, seedTime)

	h.exec(`INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at) VALUES
		(?, 'living-room', 'A television.',
		 '[{"attribute":"resolution","op":"gte","value":1080}]',
		 '[{"attribute":"video_codec","op":"eq","value":"hevc","weight":20},'||
		 '{"attribute":"hdr","op":"eq","value":true,"weight":10}]',
		 '[{"attribute":"resolution","op":"gte","value":2160},'||
		 '{"attribute":"source","op":"eq","value":"remux"}]', 1, ?, ?),
		(?, 'archival', 'Never finished.',
		 '[]',
		 '[{"attribute":"source","op":"eq","value":"remux","weight":40}]',
		 '[]', 0, ?, ?)`,
		profile1ID, seedTime, seedTime,
		profile2ID, seedTime, seedTime)

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

	// Two wants over ONE work under DIFFERENT profiles. §61 says never one
	// version per title, and this is that rule present in the fixture rather
	// than only asserted in a unit test: the living-room copy and the archival
	// copy of Arrival are two wants, and the list shape has to show both.
	//
	// The second is unmonitored, so the monitor filter has something real to
	// select and the upgrade workflow (M3-06) has a row it must skip.
	h.exec(`INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason,
		 created_at, updated_at) VALUES
		(?, 'work', ?, NULL, ?, 1, 'the good copy', ?, ?),
		(?, 'work', ?, NULL, ?, 0, '', ?, ?)`,
		desired1ID, work1ID, profile1ID, seedTime, seedTime,
		desired2ID, work1ID, profile2ID, seedTime, seedTime)

	// Acquisition state for the two seeded wants, spanning the distinction
	// §64's last three boxes turn on: one holds bytes that satisfy and is
	// waiting on placement, one holds nothing at all. A fixture where both were
	// in the same state would leave the derivation untested by the API.
	h.exec(`INSERT INTO acquisition_state
		(desired_item_id, phase, managed, content, placement, detail,
		 phase_entered_at, created_at, updated_at) VALUES
		(?, 'idle', 1, 'satisfied', 'converging', '', ?, ?, ?),
		(?, 'searching', 0, 'unknown', 'unknown', '', ?, ?, ?)`,
		desired1ID, seedTime, seedTime, seedTime,
		desired2ID, seedTime, seedTime, seedTime)

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

	// Sessions come after assets and devices, which they reference. Two of
	// them, spanning what ADR-0024's single model has to carry: a paused film
	// holding a media timestamp, and a finished book holding a page number.
	h.exec(`INSERT INTO consumption_sessions
		(id, asset_id, device_id, verb, state, progress_locator, progress_unit,
		 created_at, updated_at, started_at, ended_at) VALUES
		(?, ?, ?, 'watch', 'paused', '1284.5', 'seconds', ?, ?, ?, NULL),
		(?, ?, ?, 'read', 'completed', '312', 'page', ?, ?, ?, ?)`,
		session1ID, asset1ID, device1ID, seedTime, seedTime, seedTime,
		session2ID, asset1ID, device2ID, seedTime, seedTime, seedTime, seedTime)

	// Publication metadata against the seeded blobs, spanning the two answers
	// §69 produces: a container whose index Heyarr reads, and one it
	// deliberately does not.
	h.exec(`INSERT INTO publications (blob_hash, format, page_count, chapter_count, examined_at) VALUES
		(?, 'epub', NULL, 12, ?), (?, 'pdf', NULL, NULL, ?)`,
		blob1Hash, seedTime, blob2Hash, seedTime)

	h.exec(`INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, updated_at) VALUES
		(?, ?, 'present', 42949672960, ?, ?), (?, ?, 'corrupt', 0, NULL, ?)`,
		blob1Hash, peerID, seedTime, seedTime, blob2Hash, peerID, seedTime)

	h.exec(`INSERT INTO jobs (id, type, payload, state, priority, dedupe_key, required_capability,
			run_after, attempts, max_attempts, last_error, created_at, updated_at, finished_at) VALUES
		(?, 'scan_library', '{"root_id":"`+rootID+`"}', 'pending', 100, 'scan_library:seeded', '',
			?, 0, 5, NULL, ?, ?, NULL),
		(?, 'ingest_artifact', '{"path":"/srv/films/Broken.mkv"}', 'dead', 100, NULL, '',
			?, 5, 5, 'the file could not be read', ?, ?, ?)`,
		job1ID, seedTime, seedTime, seedTime,
		job2ID, seedTime, seedTime, seedTime, seedTime)

	return h
}

// decode issues a GET and unmarshals the body, failing on any non-200.
func decode(t *testing.T, h *harness, path string, into any) {
	t.Helper()
	resp := h.get(path)
	raw := h.body(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("GET %s returned undecodable JSON: %v\n%s", path, err, raw)
	}
}
