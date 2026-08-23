package catalog_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/rarebit-one/heyarr-core/internal/peer/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// M4-13's acceptance for the store itself (§52).
//
// Two rules shape every test in this file.
//
// First: the read path's refusal must come from the STORAGE LAYER. A test that
// asserted "no code in this package writes through a read handle" would pass
// forever and catch nothing, because the thing it is protecting against is the
// code somebody adds next week. So the assertions below attempt a real write
// and require SQLite to be the one that says no.
//
// Second: absent and empty are never the same answer. A peer that has never
// built a snapshot must be distinguishable from one holding a snapshot of an
// empty library, because in Milestone 7 those mean "I cannot help you" and
// "the library is empty".

var (
	epoch  = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	stampA = epoch
)

func newStore(t *testing.T) (*catalog.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog-snapshot.db")
	store, err := catalog.Open(context.Background(), catalog.Options{Path: path})
	if err != nil {
		t.Fatalf("opening the snapshot store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

// seed is one small, referentially closed catalogue.
func seed(version int64, generated time.Time, works ...catalog.Work) *catalog.Snapshot {
	snap := &catalog.Snapshot{
		Meta: catalog.Meta{
			ControllerID: "controller-a",
			Version:      version,
			GeneratedAt:  generated,
			Kind:         catalog.KindFull,
			Watermark:    generated,
		},
		Libraries: []catalog.Library{
			{ID: "lib-1", Name: "Films", ContentType: "movie", Enabled: true, CreatedAt: stampA},
		},
		LibraryRoots: []catalog.LibraryRoot{
			{ID: "root-1", LibraryID: "lib-1", Path: "/srv/films", IngestMode: "link", Enabled: true, CreatedAt: stampA},
		},
	}
	snap.Works = works
	for _, w := range works {
		snap.Editions = append(snap.Editions, catalog.Edition{
			ID: "ed-" + w.ID, WorkID: w.ID, Label: "1080p", EditionType: "web-dl",
			Attributes: "{}", CreatedAt: stampA,
		})
	}
	return snap
}

func work(id, title string) catalog.Work {
	return catalog.Work{
		ID: id, ContentType: "movie", WorkKey: "movie:" + id, Title: title,
		SortTitle: strings.ToLower(title), Attributes: "{}",
		CreatedAt: stampA, UpdatedAt: stampA,
	}
}

// A peer that has never built one reports ABSENT, not empty.
func TestANeverBuiltSnapshotIsAbsentRatherThanEmpty(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Nothing on disk at all.
	_, err := catalog.OpenReadOnly(ctx, filepath.Join(dir, "catalog-snapshot.db"))
	if !errors.Is(err, catalog.ErrNoSnapshot) {
		t.Fatalf("opening a snapshot that was never built = %v, want ErrNoSnapshot", err)
	}

	// A store whose schema exists but which has never had one applied. This is
	// the case a zero-value design gets wrong: the tables are there and every
	// one of them is empty, which reads exactly like an empty library.
	store, _ := newStore(t)
	if _, err := store.Metadata(ctx); !errors.Is(err, catalog.ErrNoSnapshot) {
		t.Fatalf("metadata of a never-applied store = %v, want ErrNoSnapshot", err)
	}
	if _, err := store.Describe(ctx, epoch); !errors.Is(err, catalog.ErrNoSnapshot) {
		t.Fatalf("describe of a never-applied store = %v, want ErrNoSnapshot", err)
	}

	// And the distinction it has to survive: a snapshot of an EMPTY library is
	// present, at a version, with an age. It is a different answer.
	empty := &catalog.Snapshot{Meta: catalog.Meta{
		ControllerID: "controller-a", Version: 1, GeneratedAt: epoch,
		Kind: catalog.KindFull, Watermark: epoch,
	}}
	if err := store.Apply(ctx, empty); err != nil {
		t.Fatal(err)
	}
	meta, err := store.Metadata(ctx)
	if err != nil {
		t.Fatalf("a snapshot of an empty library must be present: %v", err)
	}
	if meta.Version != 1 {
		t.Fatalf("version = %d, want 1", meta.Version)
	}
	contents, err := store.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := contents.Rows(); got != 0 {
		t.Fatalf("rows = %d, want 0 — the library really is empty here", got)
	}
}

// The §52 constraint, asserted mechanically: the snapshot cannot be written
// through the read path.
func TestTheReadPathCannotWriteTheSnapshot(t *testing.T) {
	ctx := context.Background()
	store, path := newStore(t)
	if err := store.Apply(ctx, seed(1, epoch, work("w-1", "Arrival"))); err != nil {
		t.Fatal(err)
	}

	reader, err := catalog.OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	if reader.Writable() {
		t.Fatal("a handle from OpenReadOnly reports itself writable")
	}

	// Every shape of write, not just the obvious one. A guard that stopped
	// UPDATE and let DELETE through would be worse than none, because the
	// tests would be green.
	writes := []struct {
		name  string
		query string
	}{
		{"update", `UPDATE snapshot_works SET title = 'Tampered' WHERE id = 'w-1'`},
		{"insert", `INSERT INTO snapshot_works (id, content_type, work_key, title, sort_title,
			attributes, created_at, updated_at) VALUES ('w-2','movie','k','T','t','{}','x','x')`},
		{"delete", `DELETE FROM snapshot_works`},
		{"meta", `UPDATE snapshot_meta SET version = 999 WHERE id = 1`},
		{"schema", `CREATE TABLE smuggled (id TEXT PRIMARY KEY) STRICT`},
		{"drop", `DROP TABLE snapshot_works`},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			_, err := reader.Exec(ctx, w.query)
			if err == nil {
				t.Fatal("the write succeeded through a read handle")
			}
			// The refusal must be SQLite's. "readonly" is the driver's word
			// for SQLITE_READONLY, and asserting on it is what distinguishes a
			// storage-layer refusal from a Go-level one that a future method
			// could bypass.
			if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
				t.Fatalf("refusal did not come from the storage layer: %v", err)
			}
			if !errors.Is(err, catalog.ErrReadOnly) {
				t.Fatalf("refusal is not ErrReadOnly: %v", err)
			}
		})
	}

	// And nothing changed. A refusal that had already written would be worse
	// than one that had not refused at all.
	contents, err := reader.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents.Works) != 1 || contents.Works[0].Title != "Arrival" {
		t.Fatalf("contents changed under a refused write: %+v", contents.Works)
	}

	// Apply through a read handle is refused too — belt to the storage layer's
	// braces, and the message names the reason.
	err = reader.Apply(ctx, seed(2, epoch.Add(time.Hour), work("w-1", "Arrival")))
	if !errors.Is(err, catalog.ErrReadOnly) {
		t.Fatalf("Apply through a read handle = %v, want ErrReadOnly", err)
	}
	if _, err := catalog.NewRefresher(reader, nil); !errors.Is(err, catalog.ErrReadOnly) {
		t.Fatalf("NewRefresher on a read handle = %v, want ErrReadOnly", err)
	}
}

// Version increases monotonically, and a snapshot that does not advance is
// refused rather than applied.
func TestSnapshotVersionsAdvanceAndNeverGoBackwards(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)

	for _, v := range []int64{1, 2, 3} {
		snap := seed(v, epoch.Add(time.Duration(v)*time.Hour), work("w-1", "Arrival"))
		if err := store.Apply(ctx, snap); err != nil {
			t.Fatalf("applying version %d: %v", v, err)
		}
		meta, err := store.Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if meta.Version != v {
			t.Fatalf("after applying version %d the store reports %d", v, meta.Version)
		}
	}

	// Backwards, and sideways. Both are the same failure — a peer that
	// silently accepted either would go on serving an older catalogue while
	// reporting a version that says otherwise.
	for _, v := range []int64{3, 2, 1} {
		err := store.Apply(ctx, seed(v, epoch.Add(9*time.Hour), work("w-1", "Arrival")))
		if !errors.Is(err, catalog.ErrStaleSnapshot) {
			t.Fatalf("applying version %d over 3 = %v, want ErrStaleSnapshot", v, err)
		}
	}
	meta, err := store.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Version != 3 {
		t.Fatalf("a refused apply moved the version to %d", meta.Version)
	}

	// Version zero is not a version. Absent is the absence of a snapshot.
	err = store.Apply(ctx, &catalog.Snapshot{Meta: catalog.Meta{
		ControllerID: "controller-a", Version: 0, GeneratedAt: epoch, Kind: catalog.KindFull,
	}})
	if err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("applying version 0 = %v, want a refusal naming the positive requirement", err)
	}
}

// Metadata reports the controller identity, the version and the age.
func TestMetadataReportsWhoWhichAndHowOld(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	generated := epoch
	if err := store.Apply(ctx, seed(7, generated, work("w-1", "Arrival"))); err != nil {
		t.Fatal(err)
	}

	now := generated.Add(90 * time.Minute)
	state, err := store.Describe(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if state.Meta.ControllerID != "controller-a" {
		t.Fatalf("controller = %q, want controller-a", state.Meta.ControllerID)
	}
	if state.Meta.Version != 7 {
		t.Fatalf("version = %d, want 7", state.Meta.Version)
	}
	if !state.Meta.GeneratedAt.Equal(generated) {
		t.Fatalf("generated_at = %s, want %s", state.Meta.GeneratedAt, generated)
	}
	if state.Age != 90*time.Minute {
		t.Fatalf("age = %s, want 1h30m0s", state.Age)
	}
}

// The builder refuses to be pointed at a control database, because that is how
// a second writer against the control plane gets created (Invariant 5).
func TestTheBuilderRefusesAControlDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "heyarr.db")

	// Make it look like one, the way it actually looks: goose's bookkeeping
	// table and the peers table.
	seedStore, err := catalog.Open(ctx, catalog.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedStore.Exec(ctx, `CREATE TABLE goose_db_version (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = catalog.Open(ctx, catalog.Options{Path: path})
	if err == nil {
		t.Fatal("the snapshot builder opened a control database")
	}
	if !strings.Contains(err.Error(), "control") {
		t.Fatalf("refusal does not name the reason: %v", err)
	}
}

// An in-memory snapshot store is refused: a snapshot that vanishes on restart
// is worse than none, because the peer believes it has one.
func TestAnInMemorySnapshotStoreIsRefused(t *testing.T) {
	if _, err := catalog.Open(context.Background(), catalog.Options{Path: ":memory:"}); err == nil {
		t.Fatal("an in-memory snapshot store was accepted")
	}
}

// A snapshot store built before M5-03 is discarded, not migrated (M5-03).
//
// schemaSQL is CREATE TABLE IF NOT EXISTS, so an existing snapshot_blobs with
// the old `chunked` column would survive the CREATE and then fail on the first
// INSERT against a column that is not there — a peer that opens fine and
// cannot refresh, which is the worst of the available failures because nothing
// is red until the next snapshot arrives.
func TestASnapshotStoreFromBeforeTheManifestStateIsRebuilt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog-snapshot.db")

	// Stand up a store as an older build left it.
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, `
		CREATE TABLE snapshot_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1), controller_id TEXT NOT NULL,
			version INTEGER NOT NULL CHECK (version > 0), generated_at TEXT NOT NULL,
			kind TEXT NOT NULL CHECK (kind IN ('full', 'incremental')),
			watermark TEXT NOT NULL, applied_at TEXT NOT NULL, content_digest TEXT NOT NULL
		) STRICT;
		CREATE TABLE snapshot_blobs (
			hash TEXT PRIMARY KEY, size INTEGER NOT NULL, mime TEXT,
			chunked INTEGER NOT NULL CHECK (chunked IN (0, 1)),
			first_seen_at TEXT NOT NULL
		) STRICT;
		INSERT INTO snapshot_meta VALUES (1, 'c', 7, 'x', 'full', 'x', 'x', 'x');
		INSERT INTO snapshot_blobs VALUES ('blake3:a', 1, NULL, 0, 'x');`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := catalog.Open(ctx, catalog.Options{Path: path})
	if err != nil {
		t.Fatalf("opening a store from an older build: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The stale snapshot is gone, so the next Refresh asks for a full payload
	// rather than believing it holds version 7.
	if _, err := store.Metadata(ctx); !errors.Is(err, catalog.ErrNoSnapshot) {
		t.Errorf("metadata of a rebuilt store = %v, want ErrNoSnapshot", err)
	}

	// And the rebuilt store takes a payload in the NEW shape, which is the
	// half that would have failed silently: the old table survives a CREATE
	// TABLE IF NOT EXISTS and then refuses an INSERT naming chunk_manifest.
	rebuilt := &catalog.Snapshot{
		Meta: catalog.Meta{
			ControllerID: "c", Version: 1, Kind: catalog.KindFull,
			GeneratedAt: time.Now().UTC(), Watermark: time.Now().UTC(),
		},
		Blobs: []catalog.Blob{{
			Hash: "blake3:" + strings.Repeat("a", 64), Size: 1,
			ChunkManifest: manifests.StateNotRequired, FirstSeenAt: time.Now().UTC(),
		}},
	}
	if err := store.Apply(ctx, rebuilt); err != nil {
		t.Fatalf("applying a snapshot to a rebuilt store: %v", err)
	}
	contents, err := store.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents.Blobs) != 1 || contents.Blobs[0].ChunkManifest != manifests.StateNotRequired {
		t.Errorf("the rebuilt store holds %+v", contents.Blobs)
	}
}
