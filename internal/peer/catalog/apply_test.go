package catalog_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/catalog"
)

// idsOf is the complete id set an incremental payload has to carry.
func idsOf(snap *catalog.Snapshot) map[string][]string {
	ids := map[string][]string{}
	for _, table := range catalog.Covered() {
		ids[table] = []string{}
	}
	for _, l := range snap.Libraries {
		ids["snapshot_libraries"] = append(ids["snapshot_libraries"], l.ID)
	}
	for _, r := range snap.LibraryRoots {
		ids["snapshot_library_roots"] = append(ids["snapshot_library_roots"], r.ID)
	}
	for _, w := range snap.Works {
		ids["snapshot_works"] = append(ids["snapshot_works"], w.ID)
	}
	for _, e := range snap.Editions {
		ids["snapshot_editions"] = append(ids["snapshot_editions"], e.ID)
	}
	for _, b := range snap.Blobs {
		ids["snapshot_blobs"] = append(ids["snapshot_blobs"], b.Hash)
	}
	for _, a := range snap.Assets {
		ids["snapshot_assets"] = append(ids["snapshot_assets"], a.ID)
	}
	return ids
}

// The digest distinguishes equal counts of different rows.
//
// This is the property the acceptance condition "compare row sets, not counts"
// rests on, and it is asserted here rather than assumed there.
func TestEqualCountsOfDifferentRowsDoNotMatch(t *testing.T) {
	a := seed(1, epoch, work("w-1", "Arrival"), work("w-2", "Dune"))
	b := seed(1, epoch, work("w-1", "Arrival"), work("w-3", "Solaris"))

	if a.Rows() != b.Rows() {
		t.Fatalf("the fixture is wrong: %d rows against %d", a.Rows(), b.Rows())
	}
	if a.ContentDigest() == b.ContentDigest() {
		t.Fatal("two catalogues with the same number of DIFFERENT rows produced the same digest")
	}

	// And the digest excludes the metadata, so an incremental refresh and a
	// full rebuild are comparable at all.
	c := seed(99, epoch.Add(72*time.Hour), work("w-1", "Arrival"), work("w-2", "Dune"))
	c.Meta.Kind = catalog.KindIncremental
	if a.ContentDigest() != c.ContentDigest() {
		t.Fatal("the content digest changed with the metadata; it must describe contents only")
	}
}

// An incremental refresh and a full rebuild of the same catalogue state produce
// identical snapshots.
func TestIncrementalAndFullConvergeOnIdenticalContents(t *testing.T) {
	ctx := context.Background()

	// The state both paths must arrive at: w-1 renamed, w-2 gone, w-3 new.
	final := seed(1, epoch,
		func() catalog.Work {
			w := work("w-1", "Arrival (Remastered)")
			w.UpdatedAt = epoch.Add(time.Hour)
			return w
		}(),
		work("w-3", "Solaris"),
	)

	// Path A: a full rebuild from nothing.
	fullStore, _ := newStore(t)
	fullSnap := *final
	fullSnap.Meta = catalog.Meta{
		ControllerID: "controller-a", Version: 1, GeneratedAt: epoch.Add(2 * time.Hour),
		Kind: catalog.KindFull, Watermark: epoch.Add(2 * time.Hour),
	}
	if err := fullStore.Apply(ctx, &fullSnap); err != nil {
		t.Fatal(err)
	}

	// Path B: an earlier state, then an incremental refresh onto it.
	incStore, _ := newStore(t)
	before := seed(1, epoch, work("w-1", "Arrival"), work("w-2", "Dune"))
	if err := incStore.Apply(ctx, before); err != nil {
		t.Fatal(err)
	}
	beforeContents, err := incStore.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if beforeContents.ContentDigest() == fullSnap.ContentDigest() {
		t.Fatal("the fixture is wrong: the incremental store already matched the target")
	}

	// The incremental payload carries only what changed, plus every id.
	delta := &catalog.Snapshot{
		Meta: catalog.Meta{
			ControllerID: "controller-a", Version: 2, GeneratedAt: epoch.Add(2 * time.Hour),
			Kind: catalog.KindIncremental, Watermark: epoch.Add(2 * time.Hour),
		},
		Works:    final.Works,
		Editions: []catalog.Edition{final.Editions[1]}, // ed-w-3 is the new one
		IDs:      idsOf(final),
	}
	if err := incStore.Apply(ctx, delta); err != nil {
		t.Fatal(err)
	}

	fullContents, err := fullStore.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	incContents, err := incStore.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fullContents.ContentDigest() != incContents.ContentDigest() {
		t.Fatalf("incremental and full diverged:\n  full: %+v\n  incremental: %+v",
			fullContents.Works, incContents.Works)
	}
	// Row sets, not counts — belt to the digest's braces, and the assertion
	// that reads usefully when it fails.
	assertSameWorks(t, fullContents.Works, incContents.Works)
	if len(incContents.Works) != 2 {
		t.Fatalf("works = %d, want 2", len(incContents.Works))
	}
	for _, w := range incContents.Works {
		if w.ID == "w-2" {
			t.Fatal("the incremental refresh did not prune the deleted work")
		}
	}
}

// An incremental payload that omits a table's id set is refused, because absent
// and empty mean opposite things there.
func TestAnIncrementalPayloadWithoutIDsIsRefused(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	if err := store.Apply(ctx, seed(1, epoch, work("w-1", "Arrival"))); err != nil {
		t.Fatal(err)
	}

	err := store.Apply(ctx, &catalog.Snapshot{Meta: catalog.Meta{
		ControllerID: "controller-a", Version: 2, GeneratedAt: epoch.Add(time.Hour),
		Kind: catalog.KindIncremental, Watermark: epoch.Add(time.Hour),
	}})
	if err == nil || !strings.Contains(err.Error(), "id set") {
		t.Fatalf("an incremental payload with no id set = %v, want a refusal naming it", err)
	}

	// A payload that carries SOME tables' ids is refused too: the missing one
	// would otherwise be silently kept forever.
	partial := idsOf(seed(1, epoch, work("w-1", "Arrival")))
	delete(partial, "snapshot_works")
	err = store.Apply(ctx, &catalog.Snapshot{
		Meta: catalog.Meta{
			ControllerID: "controller-a", Version: 2, GeneratedAt: epoch.Add(time.Hour),
			Kind: catalog.KindIncremental, Watermark: epoch.Add(time.Hour),
		},
		IDs: partial,
	})
	if err == nil || !strings.Contains(err.Error(), "snapshot_works") {
		t.Fatalf("a partial id set = %v, want a refusal naming the missing table", err)
	}
}

// A half-applied snapshot is a fact about no moment at all, so a failed apply
// leaves the previous one intact — version and contents together.
func TestAFailedApplyLeavesThePreviousSnapshotIntact(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	if err := store.Apply(ctx, seed(1, epoch, work("w-1", "Arrival"))); err != nil {
		t.Fatal(err)
	}
	before, err := store.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// An edition referring to a work that is not in the payload. Foreign keys
	// are ON in the snapshot store precisely so this cannot land.
	broken := seed(2, epoch.Add(time.Hour), work("w-1", "Arrival"))
	broken.Editions = append(broken.Editions, catalog.Edition{
		ID: "ed-orphan", WorkID: "w-missing", Attributes: "{}", CreatedAt: stampA,
	})
	if err := store.Apply(ctx, broken); err == nil {
		t.Fatal("a snapshot with a dangling edition was applied")
	}

	meta, err := store.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Version != 1 {
		t.Fatalf("version moved to %d after a failed apply", meta.Version)
	}
	after, err := store.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.ContentDigest() != after.ContentDigest() {
		t.Fatal("a failed apply changed the contents")
	}
}

// A refresher asks from the version it holds, and from zero when it holds none.
func TestRefreshAsksFromWhatItHoldsAndNeverFromAnEmptySnapshot(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)

	asked := []int64{}
	fetch := fetcherFunc(func(_ context.Context, holding int64, _ bool) (*catalog.Snapshot, error) {
		asked = append(asked, holding)
		return seed(int64(len(asked)), epoch.Add(time.Duration(len(asked))*time.Hour),
			work("w-1", "Arrival")), nil
	})
	refresher, err := catalog.NewRefresher(store, fetch)
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if _, err := refresher.Refresh(ctx, false); err != nil {
			t.Fatal(err)
		}
	}
	want := []int64{0, 1, 2}
	if len(asked) != len(want) {
		t.Fatalf("asked %v, want %v", asked, want)
	}
	for i := range want {
		if asked[i] != want[i] {
			t.Fatalf("asked %v, want %v", asked, want)
		}
	}
}

// A refresher over a fetcher that fails leaves the snapshot alone.
func TestRefreshDoesNotDisturbTheSnapshotWhenTheControllerIsUnreachable(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	if err := store.Apply(ctx, seed(4, epoch, work("w-1", "Arrival"))); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("the controller is not answering")
	refresher, err := catalog.NewRefresher(store, fetcherFunc(
		func(context.Context, int64, bool) (*catalog.Snapshot, error) { return nil, boom }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refresher.Refresh(ctx, false); !errors.Is(err, boom) {
		t.Fatalf("refresh error = %v, want the fetcher's", err)
	}
	meta, err := store.Metadata(ctx)
	if err != nil {
		t.Fatalf("the snapshot vanished when a refresh failed: %v", err)
	}
	if meta.Version != 4 {
		t.Fatalf("version = %d after a failed refresh, want 4", meta.Version)
	}
}

type fetcherFunc func(context.Context, int64, bool) (*catalog.Snapshot, error)

func (f fetcherFunc) Fetch(ctx context.Context, holding int64, full bool) (*catalog.Snapshot, error) {
	return f(ctx, holding, full)
}

func assertSameWorks(t *testing.T, want, got []catalog.Work) {
	t.Helper()
	index := func(ws []catalog.Work) map[string]catalog.Work {
		m := map[string]catalog.Work{}
		for _, w := range ws {
			m[w.ID] = w
		}
		return m
	}
	a, b := index(want), index(got)
	for id, w := range a {
		other, ok := b[id]
		if !ok {
			t.Errorf("work %s is missing", id)
			continue
		}
		if other.Title != w.Title || other.SortTitle != w.SortTitle {
			t.Errorf("work %s = %q/%q, want %q/%q", id, other.Title, other.SortTitle, w.Title, w.SortTitle)
		}
	}
	for id := range b {
		if _, ok := a[id]; !ok {
			t.Errorf("work %s is present and should not be", id)
		}
	}
}
