package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// harness is a real database, a real CAS and a real pipeline. Storage tests use
// real filesystems here rather than mocks, because every interesting property
// of ingest is a property of what actually lands on disk.
type harness struct {
	t        *testing.T
	db       *sqlite.DB
	cas      *cas.FS
	catalog  *catalog.Catalog
	events   *events.Log
	pipeline *ingest.Pipeline
	rootDir  string
	rootID   string
	libID    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	store, err := cas.OpenFS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("opening cas: %v", err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: "test-peer", PeerSite: "test-site",
	})
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := ingest.New(ingest.Options{
		Store:      NewCASByteStore(store),
		Catalog:    cat,
		Identifier: identification.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}

	h := &harness{
		t: t, db: db, cas: store, catalog: cat, events: eventLog,
		pipeline: pipeline, rootDir: filepath.Join(dir, "library"),
	}
	if err := os.MkdirAll(h.rootDir, 0o750); err != nil {
		t.Fatal(err)
	}
	h.libID, h.rootID = h.addLibrary("movies", identification.Movie, h.rootDir, "copy")
	return h
}

// addLibrary writes the library and root rows directly. Creating them through
// the API is issue #15's job; this test needs them to exist, not to exercise
// how they got there.
func (h *harness) addLibrary(name, contentType, path, mode string) (libID, rootID string) {
	h.t.Helper()
	libID = uuid.Must(uuid.NewV7()).String()
	rootID = uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := h.db.Writer().ExecContext(h.t.Context(),
		`INSERT INTO libraries (id, name, content_type, created_at) VALUES (?, ?, ?, ?)`,
		libID, name, contentType, now); err != nil {
		h.t.Fatalf("creating library: %v", err)
	}
	if _, err := h.db.Writer().ExecContext(h.t.Context(),
		`INSERT INTO library_roots (id, library_id, path, ingest_mode, created_at) VALUES (?, ?, ?, ?, ?)`,
		rootID, libID, path, mode, now); err != nil {
		h.t.Fatalf("creating library root: %v", err)
	}
	return libID, rootID
}

func (h *harness) write(relPath, contents string) string {
	h.t.Helper()
	full := filepath.Join(h.rootDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o640); err != nil {
		h.t.Fatal(err)
	}
	return full
}

func (h *harness) ingest(relPath string) ingest.Result {
	h.t.Helper()
	res, err := h.pipeline.Ingest(h.t.Context(), ingest.Request{
		RootID:     h.rootID,
		SourcePath: filepath.Join(h.rootDir, filepath.FromSlash(relPath)),
		RelPath:    relPath,
	})
	if err != nil {
		h.t.Fatalf("ingesting %s: %v", relPath, err)
	}
	return res
}

func (h *harness) count(table string) int {
	h.t.Helper()
	var n int
	// #nosec G202 -- table is a constant supplied by the test, never input.
	if err := h.db.Reader().QueryRowContext(h.t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		h.t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

func (h *harness) eventsOfType(eventType string) []events.Event {
	h.t.Helper()
	got, err := h.events.Since(h.t.Context(), 0, []string{eventType}, 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

func payloadField(t *testing.T, e events.Event, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatalf("decoding %s payload: %v", e.Type, err)
	}
	return m[key]
}

// The headline idempotency property. The job queue WILL re-run this handler
// (ADR-0008), so "ran twice" has to be indistinguishable from "ran once",
// except in the log.
func TestIngestingTheSameFileTwiceConvergesOnOneOfEverything(t *testing.T) {
	h := newHarness(t)
	h.write("Movie Title (2019)/Movie Title (2019) - 2160p.mkv", "the same bytes")

	first := h.ingest("Movie Title (2019)/Movie Title (2019) - 2160p.mkv")
	second := h.ingest("Movie Title (2019)/Movie Title (2019) - 2160p.mkv")

	for table, want := range map[string]int{"blobs": 1, "assets": 1, "replicas": 1, "works": 1, "editions": 1} {
		if got := h.count(table); got != want {
			t.Errorf("%s = %d, want %d", table, got, want)
		}
	}
	if !first.BlobCreated {
		t.Error("the first ingest did not create the blob")
	}
	if !first.AssetCreated {
		t.Error("the first ingest did not create the asset")
	}
	if second.BlobCreated || second.AssetCreated || second.WorkCreated || second.EditionCreated {
		t.Errorf("the second ingest created something: %+v", second)
	}
	if first.AssetID != second.AssetID || first.BlobHash != second.BlobHash {
		t.Errorf("the second ingest resolved different rows: %+v vs %+v", first, second)
	}

	completed := h.eventsOfType(events.TypeIngestCompleted)
	if len(completed) != 2 {
		t.Fatalf("want two ingest.completed events, got %d", len(completed))
	}
	if got := payloadField(t, completed[0], "deduplicated"); got != false {
		t.Errorf("first ingest.completed deduplicated = %v, want false", got)
	}
	if got := payloadField(t, completed[1], "deduplicated"); got != true {
		t.Errorf("second ingest.completed deduplicated = %v, want true", got)
	}
	if n := len(h.eventsOfType(events.TypeBlobCreated)); n != 1 {
		t.Errorf("blob.created emitted %d times, want 1 — it is emitted only when the blob is new", n)
	}
	if n := len(h.eventsOfType(events.TypeAssetCreated)); n != 1 {
		t.Errorf("content.asset.created emitted %d times, want 1", n)
	}
}

// Two paths, identical bytes: one blob, two assets. Deduplication is a property
// of the bytes, and an asset is a place those bytes were found (§13).
func TestTwoPathsWithIdenticalBytesShareOneBlob(t *testing.T) {
	h := newHarness(t)
	const contents = "identical bytes in two places"
	h.write("Movie A (2001)/Movie A (2001).mkv", contents)
	h.write("Movie B (2002)/Movie B (2002).mkv", contents)

	a := h.ingest("Movie A (2001)/Movie A (2001).mkv")
	b := h.ingest("Movie B (2002)/Movie B (2002).mkv")

	if a.BlobHash != b.BlobHash {
		t.Fatalf("identical bytes hashed differently: %s vs %s", a.BlobHash, b.BlobHash)
	}
	if got := h.count("blobs"); got != 1 {
		t.Errorf("blobs = %d, want 1", got)
	}
	if got := h.count("assets"); got != 2 {
		t.Errorf("assets = %d, want 2", got)
	}
	if got := h.count("replicas"); got != 1 {
		t.Errorf("replicas = %d, want 1 — one blob on one peer", got)
	}
	if a.AssetID == b.AssetID {
		t.Error("the two paths collapsed into one asset")
	}
	if !b.Deduplicated {
		t.Error("the second ingest did not report the bytes as deduplicated")
	}
	if b.BlobCreated {
		t.Error("the second ingest created a second blob row")
	}
}

// The acceptance criterion in full: a fault after the CAS write and before the
// commit leaves an orphan the GC can reclaim, and NO partial database state.
// Injected at every stage, because "it rolls back" is a claim about the last
// stage anyone happened to test.
func TestAFaultBeforeCommitLeavesAnOrphanAndNoPartialState(t *testing.T) {
	for _, stage := range []string{"blob", "work", "edition", "asset", "replica", "commit"} {
		t.Run("fault after "+stage, func(t *testing.T) {
			h := newHarness(t)
			h.write("Movie Title (2019)/Movie Title (2019).mkv", "bytes that must be orphaned")

			boom := errors.New("injected fault")
			faulting, err := catalog.New(catalog.Options{
				DB: h.db, Events: h.events, PeerName: "test-peer", PeerSite: "test-site",
				RecordFault: func(s string) error {
					if s == stage {
						return boom
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			pipeline, err := ingest.New(ingest.Options{
				Store:      NewCASByteStore(h.cas),
				Catalog:    faulting,
				Identifier: identification.Default(),
			})
			if err != nil {
				t.Fatal(err)
			}

			// Resolve the self peer first, in its own transaction. It emits
			// peer.registered, which is a real state transition and has
			// nothing to do with this ingest — snapshotting after it is what
			// keeps the assertion about ingest rather than about startup.
			if _, err := faulting.SelfPeer(t.Context()); err != nil {
				t.Fatal(err)
			}
			eventsBefore := h.count("events")

			_, err = pipeline.Ingest(t.Context(), ingest.Request{
				RootID:     h.rootID,
				SourcePath: filepath.Join(h.rootDir, "Movie Title (2019)", "Movie Title (2019).mkv"),
				RelPath:    "Movie Title (2019)/Movie Title (2019).mkv",
			})
			if !errors.Is(err, boom) {
				t.Fatalf("want the injected fault back, got %v", err)
			}

			for _, table := range []string{"blobs", "assets", "replicas", "works", "editions"} {
				if got := h.count(table); got != 0 {
					t.Errorf("%s = %d after a rolled-back ingest, want 0 — partial state survived", table, got)
				}
			}
			if got := h.count("events"); got != eventsBefore {
				t.Errorf("events = %d after a rolled-back ingest, want %d — the log recorded "+
					"a transition that did not happen", got, eventsBefore)
			}

			// The bytes ARE in the store, unreferenced. That is the designed
			// shape: an orphan is reclaimable, a dangling reference is not
			// (ADR-0018).
			var found int
			if err := h.cas.Walk(t.Context(), func(cas.Descriptor) error { found++; return nil }); err != nil {
				t.Fatal(err)
			}
			if found != 1 {
				t.Fatalf("want exactly one orphaned blob in the store, found %d", found)
			}
		})
	}
}

// A file replaced in place keeps its asset and gains a new blob. The old blob
// falls out of reference and the GC reclaims it after its grace window — it is
// never unlinked inline (ADR-0018).
func TestAReplacedFileKeepsItsAssetAndGainsANewBlob(t *testing.T) {
	h := newHarness(t)
	h.write("Movie Title (2019)/Movie Title (2019).mkv", "first cut")
	first := h.ingest("Movie Title (2019)/Movie Title (2019).mkv")

	h.write("Movie Title (2019)/Movie Title (2019).mkv", "the remaster, longer")
	second := h.ingest("Movie Title (2019)/Movie Title (2019).mkv")

	if first.AssetID != second.AssetID {
		t.Errorf("the asset changed identity: %s then %s", first.AssetID, second.AssetID)
	}
	if first.BlobHash == second.BlobHash {
		t.Fatal("different bytes produced the same hash")
	}
	if got := h.count("blobs"); got != 2 {
		t.Errorf("blobs = %d, want 2 — the superseded blob is retained for the GC", got)
	}
	if got := h.count("assets"); got != 1 {
		t.Errorf("assets = %d, want 1", got)
	}

	var current string
	if err := h.db.Reader().QueryRowContext(t.Context(),
		`SELECT blob_hash FROM assets WHERE id = ?`, second.AssetID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != second.BlobHash {
		t.Errorf("asset points at %s, want the new blob %s", current, second.BlobHash)
	}
}

func TestIdentificationIsRecordedOnEveryAsset(t *testing.T) {
	h := newHarness(t)
	h.write("Movie Title (2019)/Movie Title (2019).mkv", "identifiable")
	h.write("¿qué?.bin", "unidentifiable")

	identified := h.ingest("Movie Title (2019)/Movie Title (2019).mkv")
	unidentified := h.ingest("¿qué?.bin")

	read := func(id string) (source, rule string) {
		t.Helper()
		if err := h.db.Reader().QueryRowContext(t.Context(),
			`SELECT identification_source, identification_rule FROM assets WHERE id = ?`, id).
			Scan(&source, &rule); err != nil {
			t.Fatal(err)
		}
		return source, rule
	}

	// Milestone 3's real identifier finds these rows by exactly these two
	// columns and re-resolves them, rather than guessing which rows it may
	// touch (M1-11).
	source, rule := read(identified.AssetID)
	if source != identification.SourcePathHeuristic {
		t.Errorf("identification_source = %q, want %q", source, identification.SourcePathHeuristic)
	}
	if rule == "" {
		t.Error("no rule name was recorded for an identified asset")
	}

	source, _ = read(unidentified.AssetID)
	if source != identification.SourceUnidentified {
		t.Errorf("unidentified asset's source = %q, want %q", source, identification.SourceUnidentified)
	}
}

func TestTheSelfPeerIsCreatedExactlyOnceUnderConcurrency(t *testing.T) {
	h := newHarness(t)

	const callers = 8
	ids := make([]string, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A fresh catalog per caller: the memoised id would otherwise make
			// this a test of a mutex rather than of the database constraint.
			c, err := catalog.New(catalog.Options{
				DB: h.db, Events: h.events, PeerName: "test-peer", PeerSite: "test-site",
			})
			if err != nil {
				t.Error(err)
				return
			}
			id, err := c.SelfPeer(context.Background())
			if err != nil {
				t.Errorf("SelfPeer: %v", err)
				return
			}
			ids[i] = id
		}()
	}
	wg.Wait()

	if got := h.count("peers"); got != 1 {
		t.Fatalf("peers = %d, want 1 — the unique index on is_self is what makes this safe to race", got)
	}
	for i, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("caller %d resolved %q, want the single self peer %q", i, id, ids[0])
		}
	}
}

func TestRootResolution(t *testing.T) {
	h := newHarness(t)

	root, err := h.catalog.Root(t.Context(), h.rootID)
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if !root.Enabled || root.LibraryID != h.libID || root.LibraryContentType != identification.Movie {
		t.Errorf("Root = %+v", root)
	}

	if _, err := h.catalog.Root(t.Context(), "nope"); !errors.Is(err, ingest.ErrRootNotFound) {
		t.Errorf("unknown root gave %v, want ErrRootNotFound", err)
	}

	// A disabled library disables its roots. Reporting that as the root being
	// disabled is what the caller can act on.
	if _, err := h.db.Writer().ExecContext(t.Context(),
		`UPDATE libraries SET enabled = 0 WHERE id = ?`, h.libID); err != nil {
		t.Fatal(err)
	}
	root, err = h.catalog.Root(t.Context(), h.rootID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Enabled {
		t.Error("a root under a disabled library reported itself enabled")
	}
}

// The wiring, end to end: enqueue the job, let the real runtime claim and run
// it, and assert the bytes landed. Without this the handler is registered in a
// map nobody has watched be read.
func TestTheHandlerRunsWhenTheQueueHandsItAJob(t *testing.T) {
	h := newHarness(t)
	h.write("Movie Title (2019)/Movie Title (2019).mkv", "delivered by the queue")

	queue, err := jobs.New(jobs.Options{Writer: h.db.Writer(), Reader: h.db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	registry.RegisterFunc(ingest.JobType, IngestHandler(h.pipeline))

	runtime, err := NewRuntime(Config{
		Owner: "test", Slots: 2, PollInterval: 10 * time.Millisecond, HeartbeatInterval: time.Second,
	}, queue, registry, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	relPath := "Movie Title (2019)/Movie Title (2019).mkv"
	job, err := queue.Enqueue(t.Context(), jobs.EnqueueOptions{
		Type:      ingest.JobType,
		DedupeKey: ingest.DedupeKey(h.rootID, relPath),
		Payload: ingest.Payload{
			RootID:  h.rootID,
			Path:    filepath.Join(h.rootDir, filepath.FromSlash(relPath)),
			RelPath: relPath,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Enqueueing the same work twice while the first is live yields one job.
	again, err := queue.Enqueue(t.Context(), jobs.EnqueueOptions{
		Type:      ingest.JobType,
		DedupeKey: ingest.DedupeKey(h.rootID, relPath),
		Payload:   ingest.Payload{RootID: h.rootID, RelPath: relPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != job.ID {
		t.Errorf("the dedupe key did not suppress a duplicate: %s then %s", job.ID, again.ID)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()

	// Poll for the condition. A fixed sleep here is a bet on machine speed, and
	// this repo has lost that bet on CI three times.
	if err := until(t, 30*time.Second, func() bool {
		got, err := queue.Get(context.Background(), job.ID)
		return err == nil && got.State == jobs.Succeeded
	}); err != nil {
		got, _ := queue.Get(context.Background(), job.ID)
		cancel()
		<-done
		t.Fatalf("the job never succeeded (state %s, error %q): %v", got.State, got.LastError, err)
	}
	cancel()
	<-done

	if got := h.count("blobs"); got != 1 {
		t.Errorf("blobs = %d, want 1", got)
	}
	if got := h.count("assets"); got != 1 {
		t.Errorf("assets = %d, want 1", got)
	}
}

func until(t *testing.T, deadline time.Duration, cond func() bool) error {
	t.Helper()
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if cond() {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("condition was still false after %s", deadline)
}

func TestTheCASAdapterCarriesTheLadderResultBack(t *testing.T) {
	dir := t.TempDir()
	store, err := cas.OpenFS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(src, []byte(strings.Repeat("x", 4096)), 0o640); err != nil {
		t.Fatal(err)
	}

	adapter := NewCASByteStore(store)
	first, err := adapter.Link(t.Context(), src, ingest.Copy)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if !strings.HasPrefix(first.Hash, "blake3:") || first.Size != 4096 {
		t.Errorf("descriptor did not survive the adapter: %+v", first)
	}
	if first.Deduplicated {
		t.Error("a first ingest reported itself deduplicated")
	}

	second, err := adapter.Link(t.Context(), src, ingest.Copy)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Deduplicated {
		t.Error("re-linking the same bytes did not report deduplication")
	}
	if second.Hash != first.Hash {
		t.Errorf("hash changed on re-link: %s then %s", first.Hash, second.Hash)
	}
}

func TestTheHandlerRejectsAnUndecodablePayload(t *testing.T) {
	h := newHarness(t)
	handler := IngestHandler(h.pipeline)
	err := handler(t.Context(), jobs.Job{Type: ingest.JobType, Payload: json.RawMessage(`{"root_id": 12}`)})
	if err == nil {
		t.Fatal("an undecodable payload was accepted")
	}
	if !strings.Contains(err.Error(), "not decodable") {
		t.Errorf("error does not say what went wrong: %v", err)
	}
}
