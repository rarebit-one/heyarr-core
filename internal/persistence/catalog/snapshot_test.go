package catalog_test

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	peercatalog "github.com/rarebit-one/heyarr-core/internal/peer/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// M4-13's acceptance against a REAL controller database (§52).
//
// The peer-side store has its own tests. What these add is the half that can
// only be wrong against real rows: that the snapshot's contents are the
// controller's catalogue, that a change to the catalogue reaches the snapshot
// only when it is refreshed, and that the incremental path and the full
// rebuild agree about a catalogue neither of them made up.
//
// Contents are compared as ROW SETS. Counts would pass on a snapshot holding
// the same number of different rows, which is the exact shape of a prune that
// deleted one row and wrongly kept another.

const peerUnderTest = "peer-b"

// snapshotHarness is newHarness plus a peer to issue snapshots to and a peer
// store to materialise them into.
type snapshotHarness struct {
	*harness
	store *peercatalog.Store
}

func newSnapshotHarness(t *testing.T) *snapshotHarness {
	t.Helper()
	h := newHarness(t)
	// peer_snapshots references peers(id): the control plane will not record a
	// snapshot for a peer it does not know.
	h.exec(t, `INSERT INTO peers (id, name, site, mode, is_self, created_at)
		VALUES (?, 'peer-b', 'site-b', 'full', 0, ?)`, peerUnderTest, stamp)
	h.exec(t, `INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-1', 'Films', 'movie', 1, ?)`, stamp)
	h.exec(t, `INSERT INTO library_roots (id, library_id, path, ingest_mode, enabled, created_at)
		VALUES ('root-1', 'lib-1', '/srv/films', 'link', 1, ?)`, stamp)

	store, err := peercatalog.Open(context.Background(), peercatalog.Options{
		Path: filepath.Join(t.TempDir(), "catalog-snapshot.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &snapshotHarness{harness: h, store: store}
}

// addWork inserts a work and one edition for it, at the given instant.
func (h *snapshotHarness) addWork(t *testing.T, id, title, at string) {
	t.Helper()
	h.exec(t, `INSERT INTO works (id, content_type, work_key, title, sort_title, year,
			attributes, created_at, updated_at)
		VALUES (?, 'movie', ?, ?, ?, 2016, '{}', ?, ?)`,
		id, "movie:"+id, title, strings.ToLower(title), at, at)
	h.exec(t, `INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at)
		VALUES (?, ?, '1080p', 'web-dl', 'en', '{}', ?)`, "ed-"+id, id, at)
}

// build asks the controller for this peer's next snapshot.
func (h *snapshotHarness) build(t *testing.T, holding int64, full bool) *peercatalog.Snapshot {
	t.Helper()
	snap, err := h.cat.BuildSnapshot(context.Background(), catalog.SnapshotRequest{
		PeerID: peerUnderTest, ControllerID: "controller-a", Holding: holding, Full: full,
	})
	if err != nil {
		t.Fatalf("building the snapshot: %v", err)
	}
	return snap
}

// refresh builds and applies, the way the peer does.
func (h *snapshotHarness) refresh(t *testing.T, full bool) *peercatalog.Snapshot {
	t.Helper()
	ctx := context.Background()
	var holding int64
	switch meta, err := h.store.Metadata(ctx); {
	case errors.Is(err, peercatalog.ErrNoSnapshot):
		holding = 0
	case err != nil:
		t.Fatal(err)
	default:
		holding = meta.Version
	}
	snap := h.build(t, holding, full)
	if err := h.store.Apply(ctx, snap); err != nil {
		t.Fatalf("applying the snapshot: %v", err)
	}
	return snap
}

// controllerIDs reads a set of ids straight out of the controller.
func (h *snapshotHarness) controllerIDs(t *testing.T, table, key string) []string {
	t.Helper()
	rows, err := h.db.Reader().Query(`SELECT ` + key + ` FROM ` + table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func snapshotIDs(snap *peercatalog.Snapshot) map[string][]string {
	out := map[string][]string{}
	for _, l := range snap.Libraries {
		out["libraries"] = append(out["libraries"], l.ID)
	}
	for _, r := range snap.LibraryRoots {
		out["library_roots"] = append(out["library_roots"], r.ID)
	}
	for _, w := range snap.Works {
		out["works"] = append(out["works"], w.ID)
	}
	for _, e := range snap.Editions {
		out["editions"] = append(out["editions"], e.ID)
	}
	for _, b := range snap.Blobs {
		out["blobs"] = append(out["blobs"], b.Hash)
	}
	for _, a := range snap.Assets {
		out["assets"] = append(out["assets"], a.ID)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

func assertSameSet(t *testing.T, what string, want, got []string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: the snapshot holds %d rows and the catalogue holds %d\n  catalogue: %v\n  snapshot:  %v",
			what, len(got), len(want), want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			// Reported as sets rather than "counts differ", because equal
			// counts of different rows is the failure this exists to catch.
			t.Fatalf("%s: row sets differ\n  catalogue: %v\n  snapshot:  %v", what, want, got)
		}
	}
}

// The snapshot's contents ARE the controller's catalogue, for every covered
// table, compared as row sets.
func TestASnapshotHoldsTheControllersCatalogueRowForRow(t *testing.T) {
	h := newSnapshotHarness(t)
	ctx := context.Background()
	h.addWork(t, "w-arrival", "Arrival", stamp)
	h.addWork(t, "w-dune", "Dune", stamp)
	h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at)
		VALUES (?, 100, 'video/x-matroska', ?)`, "blake3:"+repeat("a", 64), stamp)
	h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
			source_path, role, filename, mime, identification_source, created_at, updated_at)
		VALUES ('as-1', 'ed-w-arrival', 'lib-1', 'managed', ?, '/srv/films/a.mkv', 'primary',
			'a.mkv', 'video/x-matroska', 'path', ?, ?)`, "blake3:"+repeat("a", 64), stamp, stamp)

	h.refresh(t, false)

	contents, err := h.store.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := snapshotIDs(contents)
	for _, table := range []struct{ name, key string }{
		{"libraries", "id"},
		{"library_roots", "id"},
		{"works", "id"},
		{"editions", "id"},
		{"blobs", "hash"},
		{"assets", "id"},
	} {
		assertSameSet(t, table.name, h.controllerIDs(t, table.name, table.key), got[table.name])
	}

	// And the values, not only the keys: a snapshot holding the right ids with
	// the wrong titles is a snapshot nobody can browse.
	var arrival *peercatalog.Work
	for i := range contents.Works {
		if contents.Works[i].ID == "w-arrival" {
			arrival = &contents.Works[i]
		}
	}
	if arrival == nil {
		t.Fatal("w-arrival is missing from the snapshot")
	}
	if arrival.Title != "Arrival" || arrival.Year == nil || *arrival.Year != 2016 {
		t.Fatalf("the work came across wrong: %+v", *arrival)
	}
	if len(contents.Assets) != 1 || contents.Assets[0].BlobHash == nil {
		t.Fatalf("the blob mapping did not come across: %+v", contents.Assets)
	}
}

// A catalogue change reaches the snapshot only when it is refreshed. Stale
// first, current after.
func TestACatalogueChangeIsStaleUntilTheSnapshotIsRefreshed(t *testing.T) {
	h := newSnapshotHarness(t)
	ctx := context.Background()
	h.addWork(t, "w-arrival", "Arrival", stamp)
	first := h.refresh(t, false)
	if first.Meta.Version != 1 {
		t.Fatalf("first version = %d, want 1", first.Meta.Version)
	}

	// The catalogue moves.
	later := "2026-08-02T00:00:00Z"
	h.addWork(t, "w-dune", "Dune", later)

	// STALE FIRST. This is the half that matters: reading the snapshot does
	// not refresh it, so a design that rebuilt on every read — which would
	// pass an end-state-only assertion — fails here.
	before, err := h.store.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if containsWork(before, "w-dune") {
		t.Fatal("the snapshot already had the new work before any refresh — " +
			"it is being rebuilt on read, which is not what a snapshot is")
	}
	beforeMeta, err := h.store.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if beforeMeta.Version != 1 {
		t.Fatalf("reading the snapshot moved its version to %d", beforeMeta.Version)
	}

	// CURRENT AFTER.
	second := h.refresh(t, false)
	if second.Meta.Kind != peercatalog.KindIncremental {
		t.Fatalf("second refresh kind = %s, want incremental — the peer held the "+
			"version the controller last issued", second.Meta.Kind)
	}
	after, err := h.store.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !containsWork(after, "w-dune") {
		t.Fatal("the refreshed snapshot is still missing the new work")
	}
	assertSameSet(t, "works", h.controllerIDs(t, "works", "id"), snapshotIDs(after)["works"])
}

// A deletion reaches the snapshot too — the case an "upsert what changed"
// design silently gets wrong.
func TestADeletedWorkLeavesTheSnapshotOnRefresh(t *testing.T) {
	h := newSnapshotHarness(t)
	ctx := context.Background()
	h.addWork(t, "w-arrival", "Arrival", stamp)
	h.addWork(t, "w-dune", "Dune", stamp)
	h.refresh(t, false)

	h.exec(t, `DELETE FROM works WHERE id = 'w-dune'`)

	before, err := h.store.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !containsWork(before, "w-dune") {
		t.Fatal("the fixture is wrong: the snapshot never held w-dune")
	}

	h.refresh(t, false)
	after, err := h.store.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if containsWork(after, "w-dune") {
		t.Fatal("a deleted work survived an incremental refresh")
	}
	assertSameSet(t, "works", h.controllerIDs(t, "works", "id"), snapshotIDs(after)["works"])
	assertSameSet(t, "editions", h.controllerIDs(t, "editions", "id"), snapshotIDs(after)["editions"])
}

// An incremental refresh and a full rebuild of the same catalogue state produce
// identical snapshots.
func TestIncrementalAndFullRebuildAgreeAboutTheSameCatalogue(t *testing.T) {
	h2 := newSnapshotHarness(t)
	ctx := context.Background()
	later := "2026-08-02T00:00:00Z"

	// Version 1, then move the catalogue in every direction an incremental
	// refresh has to handle: an addition, an update and a deletion.
	h2.addWork(t, "w-arrival", "Arrival", stamp)
	h2.refresh(t, false)
	h2.addWork(t, "w-dune", "Dune", later)
	h2.exec(t, `UPDATE works SET title = 'Arrival (Remastered)', sort_title = 'arrival (remastered)',
		updated_at = ? WHERE id = 'w-arrival'`, later)
	h2.exec(t, `DELETE FROM works WHERE id = 'w1'`)
	inc := h2.refresh(t, false)
	if inc.Meta.Kind != peercatalog.KindIncremental {
		t.Fatalf("kind = %s, want incremental", inc.Meta.Kind)
	}
	incContents, err := h2.store.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// A second peer store against the SAME controller, taking the full path.
	fullStore, err := peercatalog.Open(ctx, peercatalog.Options{
		Path: filepath.Join(t.TempDir(), "full-snapshot.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fullStore.Close() }()
	fullSnap, err := h2.cat.BuildSnapshot(ctx, catalog.SnapshotRequest{
		PeerID: peerUnderTest, ControllerID: "controller-a", Holding: 0, Full: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fullSnap.Meta.Kind != peercatalog.KindFull {
		t.Fatalf("kind = %s, want full", fullSnap.Meta.Kind)
	}
	if err := fullStore.Apply(ctx, fullSnap); err != nil {
		t.Fatal(err)
	}
	fullContents, err := fullStore.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if incContents.ContentDigest() != fullContents.ContentDigest() {
		t.Fatalf("incremental and full disagree\n  incremental works: %v\n  full works: %v",
			snapshotIDs(incContents)["works"], snapshotIDs(fullContents)["works"])
	}
	assertSameSet(t, "works (incremental vs full)",
		snapshotIDs(fullContents)["works"], snapshotIDs(incContents)["works"])
}

// Versions increase monotonically across builds, and the control plane records
// what it issued.
func TestSnapshotVersionsAdvanceAcrossBuilds(t *testing.T) {
	h := newSnapshotHarness(t)
	ctx := context.Background()
	h.addWork(t, "w-arrival", "Arrival", stamp)

	var last int64
	for i := 1; i <= 4; i++ {
		snap := h.refresh(t, false)
		if snap.Meta.Version <= last {
			t.Fatalf("build %d issued version %d after %d — versions must advance", i, snap.Meta.Version, last)
		}
		last = snap.Meta.Version

		rec, err := h.cat.PeerSnapshot(ctx, peerUnderTest)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Version != snap.Meta.Version {
			t.Fatalf("the control plane records version %d, the peer was issued %d",
				rec.Version, snap.Meta.Version)
		}
		if rec.ControllerID != "controller-a" {
			t.Fatalf("controller = %q", rec.ControllerID)
		}
		stored, err := h.store.Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Version != snap.Meta.Version {
			t.Fatalf("the peer holds version %d, the controller issued %d",
				stored.Version, snap.Meta.Version)
		}
	}
	if last != 4 {
		t.Fatalf("four builds produced version %d, want 4", last)
	}
}

// A peer the controller has never issued a snapshot to has NO record — not a
// record at version zero.
func TestAPeerWithNoSnapshotHasNoRecordRatherThanAnEmptyOne(t *testing.T) {
	h := newSnapshotHarness(t)
	_, err := h.cat.PeerSnapshot(context.Background(), peerUnderTest)
	if !errors.Is(err, catalog.ErrNoPeerSnapshot) {
		t.Fatalf("PeerSnapshot for a peer that has never had one = %v, want ErrNoPeerSnapshot", err)
	}
	all, err := h.cat.AllPeerSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := all[peerUnderTest]; ok {
		t.Fatal("a peer with no snapshot appeared in the collection")
	}
}

// One event per build, and it carries what an operator needs to read it.
func TestEachBuildEmitsExactlyOneEvent(t *testing.T) {
	h := newSnapshotHarness(t)
	ctx := context.Background()
	h.addWork(t, "w-arrival", "Arrival", stamp)

	for i := 1; i <= 3; i++ {
		h.refresh(t, false)
		evs, err := h.events.Since(ctx, 0, []string{events.TypeCatalogSnapshotBuilt}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != i {
			t.Fatalf("after %d builds there are %d catalog.snapshot_built events", i, len(evs))
		}
		if evs[len(evs)-1].SubjectID != peerUnderTest {
			t.Fatalf("event subject = %q, want the peer", evs[len(evs)-1].SubjectID)
		}
		payload := string(evs[len(evs)-1].Payload)
		for _, want := range []string{"controller_id", "version", "kind", "content_digest", "peer_holding"} {
			if !strings.Contains(payload, want) {
				t.Fatalf("the event payload does not carry %s: %s", want, payload)
			}
		}
	}
}

// A build asks the control plane, and the control plane reports the age.
func TestTheControlPlaneReportsTheSnapshotsAge(t *testing.T) {
	h := newSnapshotHarness(t)
	ctx := context.Background()
	h.addWork(t, "w-arrival", "Arrival", stamp)
	h.refresh(t, false)

	rec, err := h.cat.PeerSnapshot(ctx, peerUnderTest)
	if err != nil {
		t.Fatal(err)
	}
	age := rec.Age(rec.GeneratedAt.Add(3 * time.Hour))
	if age != 3*time.Hour {
		t.Fatalf("age = %s, want 3h0m0s", age)
	}
	if rec.RowCount == 0 {
		t.Fatal("the record reports no rows for a snapshot that has some")
	}
	if rec.ContentDigest == "" {
		t.Fatal("the record carries no content digest")
	}
}

func containsWork(snap *peercatalog.Snapshot, id string) bool {
	for _, w := range snap.Works {
		if w.ID == id {
			return true
		}
	}
	return false
}
