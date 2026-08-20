package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
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
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// These are the integrity tests that run against the real catalog, the real
// SQLite schema and a real CAS on a real filesystem. The unit tests in
// internal/storagefabric/integrity pin the logic; these pin the half that only
// a database can be wrong about — reference counting, ON DELETE RESTRICT, and
// what the event log ends up saying.

// testClock is injected everywhere a grace window is read (ADR-0017).
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (h *harness) integrityOptions(clk integrity.Clock) integrity.Options {
	return integrity.Options{Store: h.cas, Catalog: h.catalog, Clock: clk}
}

func (h *harness) checker(clk integrity.Clock) *integrity.Checker {
	h.t.Helper()
	c, err := integrity.NewChecker(h.integrityOptions(clk))
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) collector(clk integrity.Clock) *integrity.Collector {
	h.t.Helper()
	c, err := integrity.NewCollector(h.integrityOptions(clk))
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

// casBytes is the number of blob files in the store and their total size. Two
// numbers rather than one, because "changed nothing" has to survive a swap of
// one file for another of the same length.
func (h *harness) casBytes() (files int, bytes int64) {
	h.t.Helper()
	if err := h.cas.Walk(h.t.Context(), func(d cas.Descriptor) error {
		files++
		bytes += d.Size
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
	return files, bytes
}

// blobFile locates a blob's file by walking, so tests never encode the store's
// private fanout (§18).
func (h *harness) blobFile(hash string) string {
	h.t.Helper()
	parsed, err := hashing.Parse(hash)
	if err != nil {
		h.t.Fatal(err)
	}
	var found string
	err = filepath.WalkDir(h.cas.Root(), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if d.Name() == parsed.Hex() && strings.Contains(path, filepath.Join("blobs", "blake3")) {
			found = path
		}
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if found == "" {
		h.t.Fatalf("no file in the store for %s", hash)
	}
	return found
}

func (h *harness) quarantineFiles() []string {
	h.t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.cas.Root(), "quarantine"))
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func (h *harness) replicaState(hash string) (state string, verifiedAt string) {
	h.t.Helper()
	var verified any
	err := h.db.Reader().QueryRowContext(h.t.Context(),
		`SELECT state, verified_at FROM replicas WHERE blob_hash = ?`, hash).Scan(&state, &verified)
	if err != nil {
		h.t.Fatalf("reading the replica of %s: %v", hash, err)
	}
	if verified != nil {
		verifiedAt = fmt.Sprint(verified)
	}
	return state, verifiedAt
}

func (h *harness) blobHashes() []string {
	h.t.Helper()
	rows, err := h.db.Reader().QueryContext(h.t.Context(), `SELECT hash FROM blobs ORDER BY hash`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// deleteAsset removes an asset row the way a future delete API will: the row
// goes, the blob does not (ADR-0018).
func (h *harness) deleteAsset(id string) {
	h.t.Helper()
	if _, err := h.db.Writer().ExecContext(h.t.Context(), `DELETE FROM assets WHERE id = ?`, id); err != nil {
		h.t.Fatalf("deleting asset %s: %v", id, err)
	}
}

// --- fsck -------------------------------------------------------------------

// The acceptance criterion, against the real catalog: truncating one CAS file
// makes a deep check report THAT blob, quarantine it, and say so in the log.
func TestDeepCheckQuarantinesTheTruncatedBlobAndRecordsIt(t *testing.T) {
	h := newHarness(t)
	h.write("Good Film (2019)/Good Film (2019).mkv", "bytes that stay exactly as they were")
	h.write("Bad Film (2020)/Bad Film (2020).mkv", "bytes an external tool will rewrite in place")
	good := h.ingest("Good Film (2019)/Good Film (2019).mkv")
	bad := h.ingest("Bad Film (2020)/Bad Film (2020).mkv")

	path := h.blobFile(bad.BlobHash)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 4); err != nil {
		t.Fatal(err)
	}

	clk := newTestClock()
	report, err := h.checker(clk).Check(t.Context(), integrity.CheckOptions{Deep: true})
	if err != nil {
		t.Fatal(err)
	}

	if report.BlobsChecked != 2 {
		t.Fatalf("checked %d blobs, want 2", report.BlobsChecked)
	}
	if report.Damage() != 1 {
		t.Fatalf("damage = %d, want exactly 1: %+v", report.Damage(), report.Findings)
	}
	finding := report.Findings[0]
	if finding.Hash != bad.BlobHash {
		t.Fatalf("reported %s, want the damaged blob %s", finding.Hash, bad.BlobHash)
	}

	// The bytes left blobs/ and arrived in quarantine/.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the corrupt blob is still in blobs/: %v", err)
	}
	q := h.quarantineFiles()
	if len(q) != 1 || !strings.HasPrefix(q[0], hashing.MustParse(bad.BlobHash).Hex()) {
		t.Fatalf("quarantine holds %v, want the damaged blob", q)
	}

	// The catalog agrees, and the good blob is untouched and stamped.
	if state, _ := h.replicaState(bad.BlobHash); state != "corrupt" {
		t.Errorf("replica state = %q, want corrupt", state)
	}
	if state, verified := h.replicaState(good.BlobHash); state != "present" || verified == "" {
		t.Errorf("the healthy replica is state=%q verified_at=%q, want present and stamped", state, verified)
	}

	// The quarantine ledger explains it. A quarantined blob nobody can account
	// for later is barely better than a deleted one.
	var (
		ledgerPath, ledgerActual string
		ledgerSize               int64
	)
	if err := h.db.Reader().QueryRowContext(t.Context(),
		`SELECT path, actual_hash, size FROM quarantine WHERE blob_hash = ?`, bad.BlobHash).
		Scan(&ledgerPath, &ledgerActual, &ledgerSize); err != nil {
		t.Fatalf("no quarantine ledger entry for %s: %v", bad.BlobHash, err)
	}
	if filepath.Base(ledgerPath) != q[0] {
		t.Errorf("the ledger points at %q, the bytes are at %q", ledgerPath, q[0])
	}
	if ledgerActual == bad.BlobHash || ledgerActual == "" {
		t.Errorf("the ledger records actual_hash = %q, which is not evidence", ledgerActual)
	}
	if ledgerSize != 4 {
		t.Errorf("the ledger records %d bytes, want the 4 that were left", ledgerSize)
	}

	corruptEvents := h.eventsOfType(events.TypeReplicaCorrupt)
	if len(corruptEvents) != 1 {
		t.Fatalf("want one %s event, got %d", events.TypeReplicaCorrupt, len(corruptEvents))
	}
	if got := payloadField(t, corruptEvents[0], "quarantine_path"); got == "" || got == nil {
		t.Error("replica.corrupt does not carry the quarantine path")
	}
}

// Deleting the bytes behind the catalog's back marks the replica missing and
// the asset with it — assets.missing_since is what tells a user the film they
// think they own is not there.
func TestCheckMarksAMissingBlobAndItsAssets(t *testing.T) {
	h := newHarness(t)
	h.write("Gone Film (2021)/Gone Film (2021).mkv", "bytes that will be removed by hand")
	res := h.ingest("Gone Film (2021)/Gone Film (2021).mkv")
	if err := os.Remove(h.blobFile(res.BlobHash)); err != nil {
		t.Fatal(err)
	}

	report, err := h.checker(newTestClock()).Check(t.Context(), integrity.CheckOptions{Deep: false})
	if err != nil {
		t.Fatal(err)
	}
	if report.Damage() != 1 || report.Findings[0].Kind != integrity.KindMissing {
		t.Fatalf("want one missing finding, got %+v", report.Findings)
	}
	if state, _ := h.replicaState(res.BlobHash); state != "missing" {
		t.Errorf("replica state = %q, want missing", state)
	}
	var missingSince any
	if err := h.db.Reader().QueryRowContext(t.Context(),
		`SELECT missing_since FROM assets WHERE id = ?`, res.AssetID).Scan(&missingSince); err != nil {
		t.Fatal(err)
	}
	if missingSince == nil {
		t.Error("the asset was not marked missing")
	}
	if got := len(h.eventsOfType(events.TypeAssetMissing)); got != 1 {
		t.Errorf("want one %s event, got %d", events.TypeAssetMissing, got)
	}
}

// --- garbage collection -----------------------------------------------------

// ADR-0018's headline: after the grace window the bytes go, before it they stay.
func TestGCFreesBytesOnlyAfterTheGraceWindow(t *testing.T) {
	const grace = 72 * time.Hour
	tests := []struct {
		name        string
		advance     time.Duration
		wantPresent bool
	}{
		{name: "before the window", advance: grace - time.Second, wantPresent: true},
		{name: "after the window", advance: grace, wantPresent: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.write("Deleted Film (2018)/Deleted Film (2018).mkv", "bytes whose only asset goes away")
			res := h.ingest("Deleted Film (2018)/Deleted Film (2018).mkv")
			h.deleteAsset(res.AssetID)

			clk := newTestClock()
			collector := h.collector(clk)
			opts := integrity.CollectOptions{Apply: true, Grace: grace}

			first, err := collector.Collect(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(first.Marked) != 1 || len(first.Reclaimed) != 0 {
				t.Fatalf("the marking sweep marked %d and reclaimed %d, want 1 and 0",
					len(first.Marked), len(first.Reclaimed))
			}

			clk.advance(tc.advance)
			second, err := collector.Collect(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if second.Considered != 1 {
				t.Fatalf("considered %d blobs, want 1", second.Considered)
			}

			files, _ := h.casBytes()
			present := files == 1
			if present != tc.wantPresent {
				t.Errorf("bytes present = %v after %s of a %s window, want %v",
					present, tc.advance, grace, tc.wantPresent)
			}
			if got := h.count("blobs"); (got == 1) != tc.wantPresent {
				t.Errorf("blobs = %d, want %v", got, tc.wantPresent)
			}
			if tc.wantPresent {
				return
			}
			if got := len(h.eventsOfType(events.TypeBlobReclaimed)); got != 1 {
				t.Errorf("want one %s event, got %d", events.TypeBlobReclaimed, got)
			}
		})
	}
}

// `heyarr gc` with no flags changes nothing — asserted on the bytes and on
// every row count, not on the collector's own opinion of itself.
func TestGCWithNoFlagsChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.write("Kept Film (2015)/Kept Film (2015).mkv", "bytes that are still referenced")
	h.write("Dropped Film (2016)/Dropped Film (2016).mkv", "bytes that are not")
	h.ingest("Kept Film (2015)/Kept Film (2015).mkv")
	dropped := h.ingest("Dropped Film (2016)/Dropped Film (2016).mkv")
	h.deleteAsset(dropped.AssetID)

	// Take the blob past its window first, so there is genuinely something a
	// non-dry run would delete. Otherwise "nothing changed" would be true for
	// the wrong reason.
	clk := newTestClock()
	if _, err := h.collector(clk).Collect(t.Context(), integrity.CollectOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	clk.advance(30 * 24 * time.Hour)

	filesBefore, bytesBefore := h.casBytes()
	counts := map[string]int{}
	for _, table := range []string{"blobs", "assets", "replicas", "works", "editions", "events", "quarantine"} {
		counts[table] = h.count(table)
	}

	result, err := h.collector(clk).Collect(t.Context(), integrity.CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if !result.DryRun {
		t.Fatal("no flags did not produce a dry run")
	}
	// Non-vacuous on two counts: the sweep looked at both blobs, and it found
	// one it would have removed. "gc removed nothing" is worthless from a
	// sweep that never ran or had nothing to do.
	if result.Considered != 2 {
		t.Fatalf("considered %d blobs, want 2", result.Considered)
	}
	if len(result.Reclaimed) != 1 {
		t.Fatalf("the dry run identified %d reclaimable blobs, want 1 — "+
			"this assertion is only meaningful when there is something to delete", len(result.Reclaimed))
	}

	filesAfter, bytesAfter := h.casBytes()
	if filesAfter != filesBefore || bytesAfter != bytesBefore {
		t.Errorf("the store changed: %d files/%d bytes before, %d/%d after",
			filesBefore, bytesBefore, filesAfter, bytesAfter)
	}
	for table, want := range counts {
		if got := h.count(table); got != want {
			t.Errorf("%s = %d after a dry run, want %d", table, got, want)
		}
	}
}

// The orphan M1-10 deliberately creates: a fault after the CAS write and before
// the commit leaves bytes with no blobs row. Nothing else will ever clean it up.
func TestGCReclaimsTheOrphanAnIngestFaultLeaves(t *testing.T) {
	h := newHarness(t)
	h.write("Faulted Film (2017)/Faulted Film (2017).mkv", "bytes written before a fault rolled the transaction back")

	boom := errors.New("injected fault")
	faulting, err := catalog.New(catalog.Options{
		DB: h.db, Events: h.events, PeerName: "test-peer", PeerSite: "test-site",
		RecordFault: func(stage string) error {
			if stage == "commit" {
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
	if _, err := pipeline.Ingest(t.Context(), ingest.Request{
		RootID:     h.rootID,
		SourcePath: filepath.Join(h.rootDir, "Faulted Film (2017)", "Faulted Film (2017).mkv"),
		RelPath:    "Faulted Film (2017)/Faulted Film (2017).mkv",
	}); !errors.Is(err, boom) {
		t.Fatalf("want the injected fault back, got %v", err)
	}

	if files, _ := h.casBytes(); files != 1 {
		t.Fatalf("want one orphaned CAS file, got %d", files)
	}
	if got := h.count("blobs"); got != 0 {
		t.Fatalf("blobs = %d, want 0 — the fault should have rolled the row back", got)
	}

	// The bytes are as old as their source, so age them past the window the
	// way a month of sitting there would.
	clk := newTestClock()
	var orphan string
	if err := h.cas.Walk(t.Context(), func(d cas.Descriptor) error {
		orphan = d.Hash.String()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	old := clk.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(h.blobFile(orphan), old, old); err != nil {
		t.Fatal(err)
	}

	result, err := h.collector(clk).Collect(t.Context(), integrity.CollectOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Untracked) != 1 || result.Untracked[0].Hash != orphan {
		t.Fatalf("the sweep reclaimed %+v, want the orphan %s", result.Untracked, orphan)
	}
	if files, _ := h.casBytes(); files != 0 {
		t.Errorf("the orphan is still on disk (%d files)", files)
	}
	if got := len(h.eventsOfType(events.TypeBlobReclaimed)); got != 1 {
		t.Errorf("want one %s event, got %d", events.TypeBlobReclaimed, got)
	}
}

// The refcount check has a second, independent enforcer: assets.blob_hash is ON
// DELETE RESTRICT, so even a collector that got its arithmetic wrong cannot
// remove a blob an asset points at. Asserted directly, because a backstop
// nobody has watched catch anything is a comment.
func TestTheDatabaseRefusesToReclaimAReferencedBlob(t *testing.T) {
	h := newHarness(t)
	h.write("Referenced Film (2014)/Referenced Film (2014).mkv", "bytes with a live asset")
	res := h.ingest("Referenced Film (2014)/Referenced Film (2014).mkv")

	err := h.catalog.Reclaim(t.Context(), hashing.MustParse(res.BlobHash), res.BlobSize, true, time.Now().UTC())
	if err == nil {
		t.Fatal("the database allowed a referenced blob to be deleted")
	}
	if got := h.count("blobs"); got != 1 {
		t.Errorf("blobs = %d, want the referenced blob to have survived", got)
	}
}

// The property that actually earns its place. A refcount bug here destroys user
// data irreversibly, so it is asserted over generated reference graphs rather
// than over one hand-built case.
func TestGCNeverRemovesABlobAnAssetStillReferences(t *testing.T) {
	const graphs = 25
	var (
		totalBlobs     int
		totalDeleted   int
		totalReclaimed int
	)
	for seed := range uint64(graphs) {
		t.Run(fmt.Sprintf("graph-%d", seed), func(t *testing.T) {
			h := newHarness(t)
			rng := rand.New(rand.NewPCG(seed, 0x5DEECE66D))
			clk := newTestClock()

			blobs, assets := h.seedRandomGraph(t, rng)
			totalBlobs += len(blobs)

			// Delete a random subset of assets. Some blobs lose every
			// reference, some keep one, some are shared and keep several.
			survivors := map[string]string{} // asset id -> blob hash
			for id, hash := range assets {
				if rng.IntN(2) == 0 {
					h.deleteAsset(id)
					totalDeleted++
					continue
				}
				survivors[id] = hash
			}

			collector := h.collector(clk)
			opts := integrity.CollectOptions{Apply: true, Grace: 24 * time.Hour}
			if _, err := collector.Collect(t.Context(), opts); err != nil {
				t.Fatal(err)
			}
			clk.advance(48 * time.Hour)
			result, err := collector.Collect(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}

			// The sweep must actually have looked at the graph. Every
			// assertion below passes trivially against a collector that
			// returned without doing anything.
			if result.Considered != len(blobs) {
				t.Fatalf("considered %d blobs, want %d", result.Considered, len(blobs))
			}
			totalReclaimed += len(result.Reclaimed)

			// 1. Every surviving asset's blob is still present, in the catalog
			//    and on disk.
			remaining := map[string]bool{}
			for _, hash := range h.blobHashes() {
				remaining[hash] = true
			}
			for id, hash := range survivors {
				if !remaining[hash] {
					t.Fatalf("asset %s references %s, which garbage collection removed", id, hash)
				}
				parsed, err := hashing.Parse(hash)
				if err != nil {
					t.Fatal(err)
				}
				has, err := h.cas.Has(t.Context(), parsed)
				if err != nil {
					t.Fatal(err)
				}
				if !has {
					t.Fatalf("asset %s references %s, whose bytes are gone", id, hash)
				}
			}

			// 2. Every reclaimed blob had a reference count of zero.
			referenced := map[string]int{}
			for _, hash := range survivors {
				referenced[hash]++
			}
			for _, c := range result.Reclaimed {
				if n := referenced[c.Hash]; n != 0 {
					t.Fatalf("reclaimed %s, which %d surviving assets reference", c.Hash, n)
				}
			}
		})
	}
	t.Logf("property coverage: %d graphs, %d blobs seeded, %d assets deleted, %d blobs reclaimed",
		graphs, totalBlobs, totalDeleted, totalReclaimed)
}

// seedRandomGraph builds a random works/editions/assets/blobs graph with shared
// blobs, and returns the blob hashes and an asset id -> blob hash map.
//
// It writes rows directly rather than going through ingest: the property under
// test is about reference counting, and generating fifty realistic filenames to
// get there would test the identifier instead.
func (h *harness) seedRandomGraph(t *testing.T, rng *rand.Rand) (blobs []string, assets map[string]string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	peerID, err := h.catalog.SelfPeer(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	blobCount := 3 + rng.IntN(6)
	for i := range blobCount {
		desc, err := h.cas.Put(t.Context(), strings.NewReader(
			fmt.Sprintf("graph blob %d %d", i, rng.Uint64())))
		if err != nil {
			t.Fatal(err)
		}
		hash := desc.Hash.String()
		if _, err := h.db.Writer().ExecContext(t.Context(),
			`INSERT INTO blobs (hash, size, first_seen_at) VALUES (?, ?, ?)
			 ON CONFLICT (hash) DO NOTHING`, hash, desc.Size, now); err != nil {
			t.Fatal(err)
		}
		if _, err := h.db.Writer().ExecContext(t.Context(),
			`INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
			 VALUES (?, ?, 'present', ?, ?) ON CONFLICT DO NOTHING`,
			hash, peerID, desc.Size, now); err != nil {
			t.Fatal(err)
		}
		blobs = append(blobs, hash)
	}

	assets = map[string]string{}
	workCount := 1 + rng.IntN(3)
	for w := range workCount {
		workID := uuid.Must(uuid.NewV7()).String()
		if _, err := h.db.Writer().ExecContext(t.Context(),
			`INSERT INTO works (id, content_type, work_key, title, sort_title, created_at, updated_at)
			 VALUES (?, 'movie', ?, ?, ?, ?, ?)`,
			workID, fmt.Sprintf("movie:generated-%s", workID), "Generated", "generated", now, now); err != nil {
			t.Fatal(err)
		}
		editionCount := 1 + rng.IntN(2)
		for e := range editionCount {
			editionID := uuid.Must(uuid.NewV7()).String()
			if _, err := h.db.Writer().ExecContext(t.Context(),
				`INSERT INTO editions (id, work_id, edition_key, created_at) VALUES (?, ?, ?, ?)`,
				editionID, workID, fmt.Sprintf("edition-%d", e), now); err != nil {
				t.Fatal(err)
			}
			assetCount := 1 + rng.IntN(4)
			for a := range assetCount {
				// Deliberately drawn with replacement: two assets sharing one
				// blob is the case a naive collector gets wrong (§13).
				hash := blobs[rng.IntN(len(blobs))]
				assetID := uuid.Must(uuid.NewV7()).String()
				sourcePath := fmt.Sprintf("/generated/%d/%d/%d/%s.mkv", w, e, a, assetID)
				if _, err := h.db.Writer().ExecContext(t.Context(),
					`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
						role, filename, identification_source, created_at, updated_at)
					 VALUES (?, ?, ?, 'managed', ?, ?, 'primary', 'generated.mkv', 'generated', ?, ?)`,
					assetID, editionID, h.libID, hash, sourcePath, now, now); err != nil {
					t.Fatal(err)
				}
				assets[assetID] = hash
			}
		}
	}
	return blobs, assets
}

// --- job handlers -----------------------------------------------------------

func TestVerifyBlobHandlerRecordsBothOutcomes(t *testing.T) {
	h := newHarness(t)
	h.write("Verified Film (2013)/Verified Film (2013).mkv", "bytes that verify cleanly")
	res := h.ingest("Verified Film (2013)/Verified Film (2013).mkv")

	checker := h.checker(newTestClock())
	handler := VerifyBlobHandler(checker, discardLogger())

	if err := handler(t.Context(), jobWithPayload(t, fmt.Sprintf(`{"hash":%q}`, res.BlobHash))); err != nil {
		t.Fatalf("verifying a healthy blob failed the job: %v", err)
	}
	if state, verified := h.replicaState(res.BlobHash); state != "present" || verified == "" {
		t.Errorf("state=%q verified_at=%q, want present and stamped", state, verified)
	}

	// Now break it. The job must still succeed: the question was answered and
	// recorded, and failing would retry a re-hash of bytes already known bad.
	path := h.blobFile(res.BlobHash)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 2); err != nil {
		t.Fatal(err)
	}
	if err := handler(t.Context(), jobWithPayload(t, fmt.Sprintf(`{"hash":%q}`, res.BlobHash))); err != nil {
		t.Fatalf("verifying a corrupt blob failed the job: %v", err)
	}
	if state, _ := h.replicaState(res.BlobHash); state != "corrupt" {
		t.Errorf("state = %q, want corrupt", state)
	}
	if got := len(h.quarantineFiles()); got != 1 {
		t.Errorf("quarantine holds %d files, want 1", got)
	}

	// A hash that is not a hash is a permanent failure, not a retry.
	if err := handler(t.Context(), jobWithPayload(t, `{"hash":"not-a-hash"}`)); err == nil {
		t.Error("a malformed hash was accepted")
	}
}

// The job payload's zero value must be a dry run. A scheduled sweep that
// deletes because a field was omitted is how a library disappears overnight.
func TestGCHandlerDefaultsToADryRun(t *testing.T) {
	h := newHarness(t)
	h.write("Sweepable Film (2012)/Sweepable Film (2012).mkv", "bytes with no surviving asset")
	res := h.ingest("Sweepable Film (2012)/Sweepable Film (2012).mkv")
	h.deleteAsset(res.AssetID)

	clk := newTestClock()
	collector := h.collector(clk)
	if _, err := collector.Collect(t.Context(), integrity.CollectOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	clk.advance(30 * 24 * time.Hour)

	filesBefore, bytesBefore := h.casBytes()
	handler := GCHandler(collector, discardLogger())
	if err := handler(t.Context(), jobWithPayload(t, `{}`)); err != nil {
		t.Fatal(err)
	}
	filesAfter, bytesAfter := h.casBytes()
	if filesAfter != filesBefore || bytesAfter != bytesBefore {
		t.Errorf("an empty gc_blobs payload deleted something: %d/%d became %d/%d",
			filesBefore, bytesBefore, filesAfter, bytesAfter)
	}
	if h.count("blobs") != 1 {
		t.Error("an empty gc_blobs payload removed a catalog row")
	}

	if err := handler(t.Context(), jobWithPayload(t, `{"apply":true}`)); err != nil {
		t.Fatal(err)
	}
	if files, _ := h.casBytes(); files != 0 {
		t.Errorf("an explicit apply left %d files", files)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func jobWithPayload(t *testing.T, payload string) jobs.Job {
	t.Helper()
	return jobs.Job{ID: uuid.Must(uuid.NewV7()).String(), Type: "test", Payload: json.RawMessage(payload)}
}
