package integrity_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// clock is the injected clock (ADR-0017). Every grace-window assertion in this
// file moves it rather than sleeping: a test that waited out a seven-day window
// would not be a test.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeCatalog is an in-memory stand-in for the control plane, so the checker
// and the collector can be tested without a database. The real implementation
// is exercised end to end in internal/worker.
type fakeCatalog struct {
	blobs map[string]*integrity.Blob

	verified   map[string]time.Time
	corrupt    map[string]integrity.Corruption
	missing    map[string]time.Time
	reclaimed  []string
	untracked  []string
	marked     int
	cleared    int
	forceKnown map[string]bool

	// The peer-shaped state ADR-0018's placement precondition reads (M4-12).
	peers    []integrity.Peer
	replicas map[string][]integrity.Replica
	evidence map[string][]integrity.Evidence
	// corrected records "hash|peer" for every lying row moved to missing.
	corrected []string
	// order interleaves evidence writes and reclaims, so the test can assert
	// that the evidence was durable BEFORE the delete rather than after it.
	order []string
}

func newFakeCatalog() *fakeCatalog {
	return &fakeCatalog{
		blobs:    map[string]*integrity.Blob{},
		verified: map[string]time.Time{},
		corrupt:  map[string]integrity.Corruption{},
		missing:  map[string]time.Time{},
	}
}

func (f *fakeCatalog) add(h hashing.Hash, size int64, refs int) {
	f.blobs[h.String()] = &integrity.Blob{Hash: h, Size: size, References: refs}
}

func (f *fakeCatalog) Blobs(context.Context) ([]integrity.Blob, error) {
	out := make([]integrity.Blob, 0, len(f.blobs))
	for _, b := range f.blobs {
		out = append(out, *b)
	}
	return out, nil
}

func (f *fakeCatalog) Blob(_ context.Context, h hashing.Hash) (integrity.Blob, error) {
	b, ok := f.blobs[h.String()]
	if !ok {
		return integrity.Blob{}, integrity.ErrUnknownBlob
	}
	return *b, nil
}

func (f *fakeCatalog) Known(_ context.Context, hashes []hashing.Hash) (map[string]bool, error) {
	out := map[string]bool{}
	for _, h := range hashes {
		if forced, ok := f.forceKnown[h.String()]; ok {
			out[h.String()] = forced
			continue
		}
		_, ok := f.blobs[h.String()]
		out[h.String()] = ok
	}
	return out, nil
}

func (f *fakeCatalog) MarkVerified(_ context.Context, h hashing.Hash, at time.Time) error {
	f.verified[h.String()] = at
	return nil
}

func (f *fakeCatalog) MarkCorrupt(_ context.Context, c integrity.Corruption, _ time.Time) error {
	f.corrupt[c.Hash.String()] = c
	return nil
}

func (f *fakeCatalog) MarkMissing(_ context.Context, h hashing.Hash, at time.Time) error {
	f.missing[h.String()] = at
	return nil
}

func (f *fakeCatalog) MarkUnreferenced(_ context.Context, hashes []hashing.Hash, at time.Time) error {
	for _, h := range hashes {
		b, ok := f.blobs[h.String()]
		if !ok || !b.UnreferencedSince.IsZero() {
			continue
		}
		b.UnreferencedSince = at
		f.marked++
	}
	return nil
}

func (f *fakeCatalog) ClearUnreferenced(_ context.Context, hashes []hashing.Hash) error {
	for _, h := range hashes {
		if b, ok := f.blobs[h.String()]; ok {
			b.UnreferencedSince = time.Time{}
			f.cleared++
		}
	}
	return nil
}

func (f *fakeCatalog) Reclaim(_ context.Context, h hashing.Hash, _ int64, tracked bool, _ time.Time) error {
	if tracked {
		delete(f.blobs, h.String())
		f.reclaimed = append(f.reclaimed, h.String())
		f.order = append(f.order, "reclaim:"+h.String())
		return nil
	}
	f.untracked = append(f.untracked, h.String())
	return nil
}

// fixture is a real CAS on a real temporary filesystem plus a fake catalog.
type fixture struct {
	t     *testing.T
	store *cas.FS
	cat   *fakeCatalog
	clock *clock

	// claimAge is how long before the CURRENT clock each peer claim was last
	// confirmed, re-applied whenever the clock moves — see fixture.claims in
	// durability_test.go.
	claimAge    map[string]time.Duration
	everStamped map[string]bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := cas.OpenFS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, store: store, cat: newFakeCatalog(), clock: newClock()}
}

func (f *fixture) options() integrity.Options {
	return integrity.Options{Store: f.store, Catalog: f.cat, Clock: f.clock}
}

func (f *fixture) checker() *integrity.Checker {
	f.t.Helper()
	c, err := integrity.NewChecker(f.options())
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) collector() *integrity.Collector {
	f.t.Helper()
	c, err := integrity.NewCollector(f.options())
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// put stores bytes and records them in the fake catalog with refs references.
func (f *fixture) put(contents string, refs int) hashing.Hash {
	f.t.Helper()
	desc, err := f.store.Put(f.t.Context(), strings.NewReader(contents))
	if err != nil {
		f.t.Fatal(err)
	}
	f.cat.add(desc.Hash, desc.Size, refs)
	return desc.Hash
}

// putUnrecorded stores bytes without a catalog row — the orphan an ingest
// fault between the CAS write and the commit leaves behind (M1-10).
func (f *fixture) putUnrecorded(contents string, age time.Duration) hashing.Hash {
	f.t.Helper()
	desc, err := f.store.Put(f.t.Context(), strings.NewReader(contents))
	if err != nil {
		f.t.Fatal(err)
	}
	f.age(f.blobPath(desc.Hash), age)
	return desc.Hash
}

// blobPath finds a blob's file by walking, so the test never encodes the
// store's fanout — the layout is private to the cas package (§18).
func (f *fixture) blobPath(h hashing.Hash) string {
	f.t.Helper()
	var found string
	err := filepath.WalkDir(f.store.Root(), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if d.Name() == h.Hex() && filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path)))) == "blake3" {
			found = path
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if found == "" {
		f.t.Fatalf("no file in the store for %s", h)
	}
	return found
}

func (f *fixture) age(path string, by time.Duration) {
	f.t.Helper()
	when := f.clock.Now().Add(-by)
	if err := os.Chtimes(path, when, when); err != nil {
		f.t.Fatal(err)
	}
}

// truncate rewrites a blob's bytes in place, which is exactly what an external
// tool does to an original the CAS shares an inode with (#43).
func (f *fixture) truncate(h hashing.Hash) {
	f.t.Helper()
	path := f.blobPath(h)
	if err := os.Chmod(path, 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Truncate(path, 3); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) quarantined() []string {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.store.Root(), "quarantine"))
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// --- checks -----------------------------------------------------------------

// The headline acceptance criterion: a truncated file must be reported as THAT
// blob, not as "some damage", and its bytes must end up in quarantine rather
// than being deleted (ADR-0018).
func TestDeepCheckNamesTheDamagedBlobAndQuarantinesIt(t *testing.T) {
	f := newFixture(t)
	healthy := f.put("a perfectly good film", 1)
	damaged := f.put("a film that will be rewritten under us", 1)
	f.truncate(damaged)

	report, err := f.checker().Check(t.Context(), integrity.CheckOptions{Deep: true})
	if err != nil {
		t.Fatal(err)
	}

	if report.BlobsChecked != 2 {
		t.Fatalf("checked %d blobs, want 2 — a check that examined nothing proves nothing", report.BlobsChecked)
	}
	if report.Damage() != 1 {
		t.Fatalf("damage = %d, want exactly 1: %+v", report.Damage(), report.Findings)
	}
	finding := report.Findings[0]
	if finding.Hash != damaged.String() {
		t.Errorf("reported %s, want the damaged blob %s", finding.Hash, damaged)
	}
	if finding.Kind != integrity.KindCorrupt {
		t.Errorf("kind = %s, want %s", finding.Kind, integrity.KindCorrupt)
	}
	if !finding.Quarantined || finding.Path == "" {
		t.Errorf("finding does not record a quarantine destination: %+v", finding)
	}

	// The bytes left blobs/ and arrived in quarantine/.
	has, err := f.store.Has(t.Context(), damaged)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("the corrupt blob is still addressable in the store")
	}
	q := f.quarantined()
	if len(q) != 1 {
		t.Fatalf("quarantine holds %v, want exactly the damaged blob", q)
	}
	if got := q[0]; len(got) < len(damaged.Hex()) || got[:len(damaged.Hex())] != damaged.Hex() {
		t.Errorf("quarantined file %q is not %s", got, damaged.Hex())
	}
	if _, ok := f.cat.corrupt[damaged.String()]; !ok {
		t.Error("the catalog was not told the replica is corrupt")
	}

	// And the healthy blob was verified rather than merely left alone.
	if _, ok := f.cat.verified[healthy.String()]; !ok {
		t.Error("the healthy blob was not stamped verified")
	}
}

// A shallow check is fast because it does not read bytes — but a length that
// already disagrees with the catalog is proof enough to spend one read on.
func TestShallowCheckEscalatesALengthMismatch(t *testing.T) {
	f := newFixture(t)
	damaged := f.put("a film that will be truncated", 1)
	f.truncate(damaged)

	report, err := f.checker().Check(t.Context(), integrity.CheckOptions{Deep: false})
	if err != nil {
		t.Fatal(err)
	}
	if report.Damage() != 1 {
		t.Fatalf("damage = %d, want 1: %+v", report.Damage(), report.Findings)
	}
	if report.Findings[0].Kind != integrity.KindCorrupt {
		t.Errorf("kind = %s, want %s — a shallow check that spots a wrong length must not "+
			"leave the corrupt bytes addressable", report.Findings[0].Kind, integrity.KindCorrupt)
	}
	if len(f.quarantined()) != 1 {
		t.Error("the shallow check reported corruption without quarantining it")
	}
}

func TestCheckReportsCatalogRowsWithNoBytes(t *testing.T) {
	f := newFixture(t)
	gone := f.put("bytes that will be removed behind Heyarr's back", 1)
	if err := f.store.Delete(t.Context(), gone); err != nil {
		t.Fatal(err)
	}

	report, err := f.checker().Check(t.Context(), integrity.CheckOptions{Deep: false})
	if err != nil {
		t.Fatal(err)
	}
	if report.Damage() != 1 || report.Findings[0].Kind != integrity.KindMissing {
		t.Fatalf("want one missing finding, got %+v", report.Findings)
	}
	if _, ok := f.cat.missing[gone.String()]; !ok {
		t.Error("the catalog was not told the replica is missing")
	}
}

func TestCheckReportsUntrackedBytesAsWasteNotDamage(t *testing.T) {
	f := newFixture(t)
	f.put("catalogued", 1)
	orphan := f.putUnrecorded("written by an ingest that never committed", 0)

	report, err := f.checker().Check(t.Context(), integrity.CheckOptions{Deep: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Damage() != 0 {
		t.Errorf("untracked bytes were reported as damage: %+v", report.Findings)
	}
	if report.Reclaimable() != 1 {
		t.Fatalf("want one reclaimable finding, got %+v", report.Findings)
	}
	if report.Findings[0].Hash != orphan.String() {
		t.Errorf("reported %s, want %s", report.Findings[0].Hash, orphan)
	}
	if report.FilesInStore != 2 {
		t.Errorf("files in store = %d, want 2", report.FilesInStore)
	}
}

func TestVerifyBlobOnAnUnknownHash(t *testing.T) {
	f := newFixture(t)
	_, err := f.checker().VerifyBlob(t.Context(), hashing.MustParse(
		"blake3:"+"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"))
	if !errors.Is(err, integrity.ErrUnknownBlob) {
		t.Fatalf("want ErrUnknownBlob, got %v", err)
	}
}

// --- collection -------------------------------------------------------------

// ADR-0018: `heyarr gc` with no flags changes nothing. Asserted here at the
// options level — the zero value of CollectOptions must be the safe one — and
// again through the CLI in internal/cli.
func TestCollectChangesNothingByDefault(t *testing.T) {
	f := newFixture(t)
	kept := f.put("referenced", 1)
	orphan := f.put("unreferenced", 0)
	f.cat.blobs[orphan.String()].UnreferencedSince = f.clock.Now().Add(-30 * 24 * time.Hour)
	f.putUnrecorded("untracked", 30*24*time.Hour)

	result, err := f.collector().Collect(t.Context(), integrity.CollectOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if !result.DryRun {
		t.Error("the zero CollectOptions was not a dry run")
	}
	// Non-vacuous: it has to have looked at the blobs to have found nothing.
	if result.Considered != 2 {
		t.Fatalf("considered %d blobs, want 2 — 'gc removed nothing' passes trivially "+
			"against a sweep that never ran", result.Considered)
	}
	if len(result.Reclaimed) != 1 || len(result.Untracked) != 1 {
		t.Fatalf("the dry run did not identify what it would reclaim: %+v", result)
	}
	if len(f.cat.reclaimed) != 0 || len(f.cat.untracked) != 0 {
		t.Errorf("a dry run reclaimed rows: %v %v", f.cat.reclaimed, f.cat.untracked)
	}
	for _, h := range []hashing.Hash{kept, orphan} {
		has, err := f.store.Has(t.Context(), h)
		if err != nil || !has {
			t.Errorf("a dry run removed the bytes of %s", h)
		}
	}
	if f.cat.marked != 0 {
		t.Error("a dry run wrote grace-window marks")
	}
}

// The grace window is the whole safety argument of ADR-0018, so it gets a table
// rather than one happy path.
func TestGraceWindow(t *testing.T) {
	const grace = 48 * time.Hour
	tests := []struct {
		name          string
		advance       time.Duration
		wantReclaimed int
		wantWaiting   int
	}{
		{name: "immediately after the mark", advance: 0, wantReclaimed: 0, wantWaiting: 1},
		{name: "halfway through the window", advance: grace / 2, wantReclaimed: 0, wantWaiting: 1},
		{name: "one second short", advance: grace - time.Second, wantReclaimed: 0, wantWaiting: 1},
		{name: "exactly at the window", advance: grace, wantReclaimed: 1, wantWaiting: 0},
		{name: "well past the window", advance: 30 * 24 * time.Hour, wantReclaimed: 1, wantWaiting: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			orphan := f.put("nothing references these bytes", 0)
			collector := f.collector()
			opts := integrity.CollectOptions{Apply: true, Grace: grace}

			// The first sweep starts the window and must reclaim nothing, ever.
			first, err := collector.Collect(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(first.Reclaimed) != 0 {
				t.Fatalf("the marking sweep reclaimed %d blobs; the window must start before "+
					"anything is freed", len(first.Reclaimed))
			}
			if len(first.Marked) != 1 {
				t.Fatalf("the marking sweep marked %d blobs, want 1", len(first.Marked))
			}

			f.clock.advance(tc.advance)
			second, err := collector.Collect(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(second.Reclaimed); got != tc.wantReclaimed {
				t.Errorf("reclaimed %d, want %d after %s of a %s window", got, tc.wantReclaimed, tc.advance, grace)
			}
			if got := len(second.Waiting); got != tc.wantWaiting {
				t.Errorf("waiting %d, want %d", got, tc.wantWaiting)
			}
			has, err := f.store.Has(t.Context(), orphan)
			if err != nil {
				t.Fatal(err)
			}
			if has == (tc.wantReclaimed == 1) {
				t.Errorf("bytes present = %v after %s, but reclaimed = %d", has, tc.advance, tc.wantReclaimed)
			}
		})
	}
}

// A blob that regains a reference gets a fresh window, not a partly spent one.
func TestARegainedReferenceRestartsTheWindow(t *testing.T) {
	f := newFixture(t)
	h := f.put("shared bytes", 0)
	collector := f.collector()
	opts := integrity.CollectOptions{Apply: true, Grace: 48 * time.Hour}

	if _, err := collector.Collect(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	if f.cat.blobs[h.String()].UnreferencedSince.IsZero() {
		t.Fatal("the first sweep did not start a window")
	}

	f.cat.blobs[h.String()].References = 1
	f.clock.advance(24 * time.Hour)
	if _, err := collector.Collect(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	if !f.cat.blobs[h.String()].UnreferencedSince.IsZero() {
		t.Fatal("a referenced blob kept its grace-window mark")
	}

	f.cat.blobs[h.String()].References = 0
	f.clock.advance(24 * time.Hour)
	restarted, err := collector.Collect(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.Reclaimed) != 0 {
		t.Error("the blob was reclaimed on a window that should have restarted")
	}
	if len(restarted.Marked) != 1 {
		t.Errorf("marked %d, want a fresh window", len(restarted.Marked))
	}
}

// Untracked bytes are reclaimable, but only once they are old enough that they
// cannot be an ingest that has written its bytes and not yet committed.
func TestUntrackedBytesWaitOutTheirAge(t *testing.T) {
	f := newFixture(t)
	fresh := f.putUnrecorded("just written by an ingest still in its transaction", 0)
	old := f.putUnrecorded("left by an ingest that faulted last month", 30*24*time.Hour)

	result, err := f.collector().Collect(t.Context(), integrity.CollectOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Untracked) != 1 || result.Untracked[0].Hash != old.String() {
		t.Fatalf("want only the old orphan reclaimed, got %+v", result.Untracked)
	}
	if result.UntrackedWaiting != 1 {
		t.Errorf("untracked waiting = %d, want 1", result.UntrackedWaiting)
	}
	if has, _ := f.store.Has(t.Context(), fresh); !has {
		t.Error("a freshly written orphan was reclaimed; that is an uncommitted ingest's bytes")
	}
	if has, _ := f.store.Has(t.Context(), old); has {
		t.Error("the old orphan was not reclaimed")
	}
}

// The snapshot the sweep walks from is stale by the time it acts on it, so
// anything old enough to delete is re-checked against the catalog first.
func TestUntrackedBytesThatGainedARowAreSpared(t *testing.T) {
	f := newFixture(t)
	old := f.putUnrecorded("an ingest committed while the sweep was walking", 30*24*time.Hour)
	f.cat.forceKnown = map[string]bool{old.String(): true}

	result, err := f.collector().Collect(t.Context(), integrity.CollectOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Untracked) != 0 {
		t.Fatalf("reclaimed bytes that had gained a catalog row: %+v", result.Untracked)
	}
	if result.UntrackedWaiting != 1 {
		t.Errorf("untracked waiting = %d, want 1", result.UntrackedWaiting)
	}
	if has, _ := f.store.Has(t.Context(), old); !has {
		t.Error("the bytes were removed despite the re-check")
	}
}

func TestPartialWritesAreSweptOnceTheyAreOldEnough(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.store.Root(), "tmp")
	for name, age := range map[string]time.Duration{
		"put-old.part":   30 * 24 * time.Hour,
		"put-fresh.part": 0,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.age(path, age)
	}

	result, err := f.collector().Collect(t.Context(), integrity.CollectOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.TempRemoved) != 1 || result.TempRemoved[0].Name != "put-old.part" {
		t.Fatalf("swept %+v, want only put-old.part", result.TempRemoved)
	}
	if _, err := os.Stat(filepath.Join(dir, "put-fresh.part")); err != nil {
		t.Error("a partial write from an in-flight ingest was swept")
	}
}

func TestANegativeGraceWindowIsRefused(t *testing.T) {
	f := newFixture(t)
	if _, err := f.collector().Collect(t.Context(), integrity.CollectOptions{
		Apply: true, Grace: -time.Hour,
	}); err == nil {
		t.Fatal("a negative grace window was accepted")
	}
}

// A referenced blob is never a candidate, whatever else is true of it.
func TestReferencedBlobsAreNeverCandidates(t *testing.T) {
	f := newFixture(t)
	referenced := f.put("still in the library", 3)
	// Even with a stale mark from when it briefly had no references.
	f.cat.blobs[referenced.String()].UnreferencedSince = f.clock.Now().Add(-365 * 24 * time.Hour)

	result, err := f.collector().Collect(t.Context(), integrity.CollectOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Reclaimed) != 0 {
		t.Fatalf("reclaimed a referenced blob: %+v", result.Reclaimed)
	}
	if result.Referenced != 1 {
		t.Errorf("referenced = %d, want 1", result.Referenced)
	}
	if has, _ := f.store.Has(t.Context(), referenced); !has {
		t.Fatal("the bytes of a referenced blob were removed")
	}
	if f.cat.cleared != 1 {
		t.Error("the stale mark was not cleared")
	}
}

// The peer-shaped half of the fake catalog (M4-12).
//
// It is deliberately dumb: it stores what it is told and hands it back. Every
// interesting decision in the durability precondition belongs to the collector,
// and a fake that made any of those decisions itself would be the thing under
// test.

func (f *fakeCatalog) Peers(context.Context) ([]integrity.Peer, error) {
	return f.peers, nil
}

func (f *fakeCatalog) Replicas(_ context.Context, h hashing.Hash) ([]integrity.Replica, error) {
	return f.replicas[h.String()], nil
}

func (f *fakeCatalog) MarkReplicaMissing(_ context.Context, h hashing.Hash, peerID string, _ time.Time) error {
	f.corrected = append(f.corrected, h.String()+"|"+peerID)
	for i, r := range f.replicas[h.String()] {
		if r.Peer.PeerID == peerID {
			f.replicas[h.String()][i].State = "missing"
		}
	}
	return nil
}

func (f *fakeCatalog) RecordDurability(_ context.Context, e integrity.Evidence) error {
	if f.evidence == nil {
		f.evidence = map[string][]integrity.Evidence{}
	}
	f.evidence[e.BlobHash.String()] = append(f.evidence[e.BlobHash.String()], e)
	// Ordering is recorded because WHEN this happens is the point: the
	// evidence must be durable before the reclaim that relies on it, since
	// replicas.blob_hash is ON DELETE CASCADE in the real schema and the
	// delete destroys the record of who else held the blob (migration 00028).
	f.order = append(f.order, "evidence:"+e.BlobHash.String())
	return nil
}

func (f *fakeCatalog) DurabilityEvidence(_ context.Context, h hashing.Hash) ([]integrity.Evidence, error) {
	return f.evidence[h.String()], nil
}
