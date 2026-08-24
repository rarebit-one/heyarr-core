package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// integrityFixture is a config file plus a database and a store behind it, so
// the CLI can be driven end to end rather than through a seam.
type integrityFixture struct {
	t          *testing.T
	configPath string
	dir        string
	store      *cas.FS
}

func newIntegrityFixture(t *testing.T) *integrityFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "heyarr.yaml")
	body := "data_dir: " + dir + "\npeer:\n  name: test\n  site: test-site\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := cas.OpenFS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	return &integrityFixture{t: t, configPath: path, dir: dir, store: store}
}

// seed puts bytes in the store and, when tracked, records the blob and an asset
// that references it. It goes through the real schema, so the reference count
// the collector reads is the one the database computes.
func (f *integrityFixture) seed(contents string, tracked bool) string {
	f.t.Helper()
	desc, err := f.store.Put(f.t.Context(), strings.NewReader(contents))
	if err != nil {
		f.t.Fatal(err)
	}
	if !tracked {
		return desc.Hash.String()
	}

	db, err := sqlite.Open(f.t.Context(), sqlite.Options{Path: filepath.Join(f.dir, "heyarr.db")})
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := sqlite.Migrate(f.t.Context(), db); err != nil {
		f.t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		f.t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{DB: db, Events: eventLog, PeerName: "test", PeerSite: "test-site"})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := cat.SelfPeer(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}

	const ts = "2026-08-20T00:00:00Z"
	exec := func(query string, args ...any) {
		f.t.Helper()
		if _, err := db.Writer().ExecContext(f.t.Context(), query, args...); err != nil {
			f.t.Fatalf("%s: %v", strings.SplitN(strings.TrimSpace(query), "\n", 2)[0], err)
		}
	}
	id := desc.Hash.Hex()[:8]
	exec(`INSERT INTO blobs (hash, size, first_seen_at) VALUES (?, ?, ?)`, desc.Hash.String(), desc.Size, ts)
	exec(`INSERT INTO libraries (id, name, content_type, created_at) VALUES (?, ?, 'movie', ?)
		ON CONFLICT (name) DO NOTHING`, "lib-"+id, "movies-"+id, ts)
	exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, created_at, updated_at)
		VALUES (?, 'movie', ?, 'Seeded', 'seeded', ?, ?)`, "work-"+id, "movie:seeded:"+id, ts, ts)
	exec(`INSERT INTO editions (id, work_id, edition_key, created_at) VALUES (?, ?, '', ?)`,
		"edition-"+id, "work-"+id, ts)
	exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, identification_source, created_at, updated_at)
		VALUES (?, ?, ?, 'managed', ?, ?, 'primary', 'seeded.mkv', 'seeded', ?, ?)`,
		"asset-"+id, "edition-"+id, "lib-"+id, desc.Hash.String(), "/seeded/"+id+".mkv", ts, ts)
	return desc.Hash.String()
}

func (f *integrityFixture) dropAsset(hash string) {
	f.t.Helper()
	db, err := sqlite.Open(f.t.Context(), sqlite.Options{Path: filepath.Join(f.dir, "heyarr.db")})
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Writer().ExecContext(f.t.Context(),
		`DELETE FROM assets WHERE blob_hash = ?`, hash); err != nil {
		f.t.Fatal(err)
	}
}

func (f *integrityFixture) blobFile(hash string) string {
	f.t.Helper()
	hex := hash[len("blake3:"):]
	var found string
	err := filepath.WalkDir(f.store.Root(), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if d.Name() == hex && strings.Contains(path, filepath.Join("blobs", "blake3")) {
			found = path
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if found == "" {
		f.t.Fatalf("no file in the store for %s", hash)
	}
	return found
}

func (f *integrityFixture) truncate(hash string, to int64) {
	f.t.Helper()
	path := f.blobFile(hash)
	if err := os.Chmod(path, 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Truncate(path, to); err != nil {
		f.t.Fatal(err)
	}
}

func (f *integrityFixture) storeFiles() (files int, bytes int64) {
	f.t.Helper()
	if err := f.store.Walk(f.t.Context(), func(d cas.Descriptor) error {
		files++
		bytes += d.Size
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	return files, bytes
}

// quarantinePattern strips the nanosecond suffix the store appends when it
// moves bytes out of the addressable tree, which is the only part of a
// quarantine path that legitimately differs between runs.
var quarantinePattern = regexp.MustCompile(`(quarantine/[0-9a-f]{64})\.\d+`)

func normaliseIntegrity(s, root string) string {
	s = strings.ReplaceAll(s, root, "<root>")
	s = quarantinePattern.ReplaceAllString(s, "$1.<nanos>")
	return normalise(s)
}

func TestFsckOnAHealthyStoreReportsNothing(t *testing.T) {
	f := newIntegrityFixture(t)
	f.seed("bytes that are exactly what they claim to be", true)

	out, _, err := run(t, context.Background(), "--config", f.configPath, "fsck", "--deep")
	if err != nil {
		t.Fatalf("fsck on a healthy store failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no problems found") {
		t.Errorf("output does not say the store is healthy:\n%s", out)
	}
	// Non-vacuous: it has to have looked at the blob to have found nothing.
	if !strings.Contains(out, "blobs checked     1") {
		t.Errorf("fsck reported success without checking anything:\n%s", out)
	}
}

// A checker that exits 0 having found corruption is worse than no checker: it
// gets wired into cron and its silence starts being trusted.
func TestFsckExitsNonZeroOnDamage(t *testing.T) {
	f := newIntegrityFixture(t)
	hash := f.seed("bytes an external tool will rewrite through a shared inode", true)
	f.truncate(hash, 5)

	out, _, err := run(t, context.Background(), "--config", f.configPath, "fsck", "--deep")
	if err == nil {
		t.Fatalf("fsck exited 0 having found corruption:\n%s", out)
	}
	if !errors.Is(err, ErrDamage) {
		t.Errorf("error = %v, want ErrDamage", err)
	}
	if !strings.Contains(out, hash) {
		t.Errorf("the report does not name the damaged blob %s:\n%s", hash, out)
	}
	if !strings.Contains(out, "quarantined at") {
		t.Errorf("the report does not say where the evidence went:\n%s", out)
	}
}

// Untracked bytes are waste, not damage, and must not fail the check.
func TestFsckDoesNotFailOnReclaimableWaste(t *testing.T) {
	f := newIntegrityFixture(t)
	f.seed("catalogued bytes", true)
	f.seed("bytes an ingest wrote and never committed", false)

	out, _, err := run(t, context.Background(), "--config", f.configPath, "fsck")
	if err != nil {
		t.Fatalf("fsck failed on reclaimable waste, which is not damage: %v\n%s", err, out)
	}
	if !strings.Contains(out, "untracked") {
		t.Errorf("the untracked bytes were not reported:\n%s", out)
	}
}

func TestFsckJSONShape(t *testing.T) {
	f := newIntegrityFixture(t)
	// Fixed contents, so the digests in the golden file are the real ones.
	hash := f.seed("the bytes that get truncated", true)
	f.seed("bytes with no catalog row", false)
	f.truncate(hash, 3)

	out, _, err := run(t, context.Background(), "--config", f.configPath, "fsck", "--deep", "--json")
	if err == nil {
		t.Fatal("fsck --json exited 0 despite damage")
	}
	var decoded map[string]any
	if jsonErr := json.Unmarshal([]byte(out), &decoded); jsonErr != nil {
		t.Fatalf("not valid JSON: %v\n%s", jsonErr, out)
	}
	testutil.Golden(t, "testdata/fsck.json", []byte(normaliseIntegrity(out, f.store.Root())))
}

// ADR-0018: `heyarr gc` with no flags changes nothing. Asserted on the bytes
// and on the row counts, and only in a situation where a non-dry run would
// certainly have deleted something.
func TestGCWithNoFlagsChangesNothing(t *testing.T) {
	f := newIntegrityFixture(t)
	hash := f.seed("bytes whose only asset is about to go away", true)
	f.dropAsset(hash)

	// Start the grace window, then move well past it, so there is genuinely
	// something an applying sweep would remove.
	if _, _, err := run(t, context.Background(), "--config", f.configPath, "gc", "--apply"); err != nil {
		t.Fatal(err)
	}
	filesBefore, bytesBefore := f.storeFiles()
	rowsBefore := f.rowCounts()

	out, _, err := run(t, context.Background(), "--config", f.configPath, "gc", "--grace", "1ns")
	if err != nil {
		t.Fatalf("gc: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dry run") {
		t.Errorf("gc without flags did not announce a dry run:\n%s", out)
	}
	if !strings.Contains(out, "reclaimed           1") {
		t.Fatalf("the dry run found nothing it would reclaim, so this assertion "+
			"would pass for the wrong reason:\n%s", out)
	}

	filesAfter, bytesAfter := f.storeFiles()
	if filesAfter != filesBefore || bytesAfter != bytesBefore {
		t.Errorf("the store changed: %d files/%d bytes became %d/%d",
			filesBefore, bytesBefore, filesAfter, bytesAfter)
	}
	for table, want := range rowsBefore {
		if got := f.rowCounts()[table]; got != want {
			t.Errorf("%s = %d after a dry run, want %d", table, got, want)
		}
	}
}

func (f *integrityFixture) rowCounts() map[string]int {
	f.t.Helper()
	db, err := sqlite.Open(f.t.Context(), sqlite.Options{Path: filepath.Join(f.dir, "heyarr.db")})
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	out := map[string]int{}
	for _, table := range []string{"blobs", "assets", "replicas", "events", "quarantine"} {
		var n int
		// #nosec G202 -- table is a constant from this test, never input.
		if err := db.Reader().QueryRowContext(f.t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			f.t.Fatalf("counting %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

func TestGCApplyFreesBytesOnlyPastTheWindow(t *testing.T) {
	f := newIntegrityFixture(t)
	hash := f.seed("bytes whose only asset goes away", true)
	f.dropAsset(hash)

	// First apply starts the window and must free nothing.
	out, _, err := run(t, context.Background(), "--config", f.configPath,
		"gc", "--apply", "--grace", "720h")
	if err != nil {
		t.Fatalf("gc --apply: %v\n%s", err, out)
	}
	if files, _ := f.storeFiles(); files != 1 {
		t.Fatalf("the marking sweep freed bytes: %d files remain", files)
	}
	if !strings.Contains(out, "window started      1") {
		t.Errorf("the sweep did not start a window:\n%s", out)
	}

	// A zero window makes the second sweep eligible without waiting.
	if _, _, err := run(t, context.Background(), "--config", f.configPath,
		"gc", "--apply", "--grace", "1ns"); err != nil {
		t.Fatal(err)
	}
	if files, _ := f.storeFiles(); files != 0 {
		t.Errorf("the bytes survived a sweep past their window: %d files remain", files)
	}
}

func TestGCJSONShape(t *testing.T) {
	f := newIntegrityFixture(t)
	hash := f.seed("bytes that lose their only reference", true)
	f.dropAsset(hash)
	f.seed("bytes with no catalog row at all", false)

	out, _, err := run(t, context.Background(), "--config", f.configPath, "gc", "--json")
	if err != nil {
		t.Fatalf("gc --json: %v\n%s", err, out)
	}
	var decoded map[string]any
	if jsonErr := json.Unmarshal([]byte(out), &decoded); jsonErr != nil {
		t.Fatalf("not valid JSON: %v\n%s", jsonErr, out)
	}
	if decoded["dry_run"] != true {
		t.Errorf("dry_run = %v in the default JSON output, want true", decoded["dry_run"])
	}
	testutil.Golden(t, "testdata/gc.json", []byte(normaliseIntegrity(out, f.store.Root())))
}

func TestGCRefusesContradictoryFlags(t *testing.T) {
	f := newIntegrityFixture(t)
	_, _, err := run(t, context.Background(), "--config", f.configPath, "gc", "--apply", "--dry-run=true")
	if err == nil {
		t.Fatal("--apply and --dry-run=true were both accepted")
	}
	if !strings.Contains(err.Error(), "contradict") {
		t.Errorf("error = %v, want it to name the contradiction", err)
	}
}

func TestGCRefusesANegativeGraceWindow(t *testing.T) {
	f := newIntegrityFixture(t)
	if _, _, err := run(t, context.Background(), "--config", f.configPath,
		"gc", "--apply", "--grace", "-1h"); err == nil {
		t.Fatal("a negative grace window was accepted")
	}
}

// The commands must work against a data directory nothing has ever started
// against — fsck is what an operator reaches for when the controller will not
// start, which is exactly when the schema may be behind.
func TestIntegrityCommandsMigrateAnUntouchedDataDir(t *testing.T) {
	f := newIntegrityFixture(t)
	for _, args := range [][]string{{"fsck"}, {"gc"}} {
		out, _, err := run(t, context.Background(), append([]string{"--config", f.configPath}, args...)...)
		if err != nil {
			t.Errorf("%v against a fresh data dir: %v\n%s", args, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, "heyarr.db")); err != nil {
		t.Errorf("no database was created: %v", err)
	}
}

// --- repair -----------------------------------------------------------------

// `fsck --repair` must say what it could not do and why. The ranged fetch this
// would pull replacement chunks over is M5-06/M5-07's, and until it is wired
// in the source is nil — which REFUSES rather than permits, so every damaged
// blob is reported unrepaired with the reason rather than quietly skipped.
func TestFsckRepairSaysWhyItCouldNotRepair(t *testing.T) {
	f := newIntegrityFixture(t)
	hash := f.seed("bytes an external tool rewrote under a shared inode", true)
	f.truncate(hash, 5)

	out, _, err := run(t, context.Background(), "--config", f.configPath, "fsck", "--deep", "--repair")
	if err == nil {
		t.Fatalf("fsck --repair exited 0 with the damage still there:\n%s", out)
	}
	if !errors.Is(err, ErrDamage) {
		t.Errorf("error = %v, want ErrDamage", err)
	}
	if !strings.Contains(out, "NOT REPAIRED") {
		t.Errorf("the repair pass did not report a failure:\n%s", out)
	}
	if !strings.Contains(out, "still damaged after the repair pass") &&
		!strings.Contains(err.Error(), "still damaged after the repair pass") {
		t.Errorf("the exit reason does not say the damage survived: %v\n%s", err, out)
	}
	// The WHY, not just the verdict. This blob has no chunk manifest, which is
	// the honest reason a chunk-scoped repair cannot touch it.
	if !strings.Contains(out, "no chunk manifest") {
		t.Errorf("the report does not say why the repair could not run:\n%s", out)
	}
}

// A healthy store is not "repaired", and the command says so rather than
// printing an empty section.
func TestFsckRepairOnAHealthyStoreRepairsNothing(t *testing.T) {
	f := newIntegrityFixture(t)
	f.seed("bytes that are exactly what they claim to be", true)

	out, _, err := run(t, context.Background(), "--config", f.configPath, "fsck", "--deep", "--repair")
	if err != nil {
		t.Fatalf("fsck --repair on a healthy store failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "nothing damaged to repair") {
		t.Errorf("the repair section is not there:\n%s", out)
	}
}

// What a successful repair prints. Driven straight at the printer with
// synthetic results, because the fetch it takes to produce a real one is not
// wired up yet — the numbers an operator reads are asserted here, and the
// repair itself is asserted in internal/storagefabric/integrity.
func TestPrintRepairsNamesWhatMovedAndWhy(t *testing.T) {
	var buf bytes.Buffer
	err := printRepairs(&buf, []integrity.RepairResult{
		{
			Hash: "blake3:aa", Outcome: integrity.OutcomeRepaired,
			ChunksTotal: 40, ChunksDamaged: 2, ChunksFetched: 2,
			BytesFetched: 2048, BlobSize: 65536,
			QuarantinePath: "/var/lib/heyarr/cas/quarantine/aa.1",
			Detail:         "replaced 2 of 40 chunks",
		},
		{
			Hash: "blake3:bb", Outcome: integrity.OutcomeUnreachable,
			ChunksTotal: 10, ChunksDamaged: 1,
			Detail: "no reachable peer holds these bytes",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"repaired          1",
		"REPAIRED",
		"2 of 40 chunks damaged, 2 fetched from a peer (2048 bytes of a 65536 byte blob)",
		"the damaged original is at /var/lib/heyarr/cas/quarantine/aa.1",
		"NOT REPAIRED",
		"no reachable peer holds these bytes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the repair report does not say %q:\n%s", want, out)
		}
	}
}
