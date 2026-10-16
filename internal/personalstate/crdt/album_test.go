package crdt

import (
	"math"
	"math/rand"
	"testing"
)

// Deterministic photo manifests for the album tests: distinct canonical blake3
// ids so ingress validation accepts them. photoDup shares photoA's plaintext hash
// under a different manifest — the same photo added from a second device.
var (
	photoA   = photo("a1", "1a")
	photoB   = photo("b2", "2b")
	photoC   = photo("c3", "3c")
	photoDup = photo("d4", "1a")
)

func photo(manifestPair, hashPair string) PhotoManifest {
	return PhotoManifest{
		Manifest:      hexBlob(manifestPair),
		Thumbnail:     hexBlob("e" + manifestPair[:1]),
		Sidecar:       hexBlob("f" + manifestPair[:1]),
		PlaintextHash: hexBlob(hashPair),
		Width:         4000, Height: 3000, TakenAt: 1_700_000_000,
	}
}

// must fails the test on a rejected local write — every write the tests mint is
// valid, so this only guards against a broken helper. Curried so a two-value call
// can be passed straight in: must(t)(a.AddItem(...)).
func must(t *testing.T) func(AlbumChange, error) AlbumChange {
	return func(c AlbumChange, err error) AlbumChange {
		t.Helper()
		if err != nil {
			t.Fatalf("local album write rejected: %v", err)
		}
		return c
	}
}

// buildAlbumChangeset returns a deterministic set of album writes exercising every
// op — create with title, caption, cover, three appends, a move, a remove, an
// add-wins re-add, a dedupe twin, and a second album that is deleted — with the set
// that results from folding them. Writers are minted once, so every permutation
// folds the SAME writes (§43).
func buildAlbumChangeset(t *testing.T) (changes []AlbumChange, want *Albums) {
	t.Helper()
	a := NewAlbums()
	add := func(c AlbumChange, err error) { changes = append(changes, must(t)(c, err)) }

	add(a.Create("alb-1", "Trip"))
	add(a.SetCaption("alb-1", "the long weekend"))
	ia := must(t)(a.AddItem("alb-1", photoA))
	changes = append(changes, ia)
	ib := must(t)(a.AddItem("alb-1", photoB))
	changes = append(changes, ib)
	ic := must(t)(a.AddItem("alb-1", photoC))
	changes = append(changes, ic)
	add(a.SetCover("alb-1", photoB.Manifest))
	add(a.MoveItem("alb-1", WriteKey{At: ic.At, Writer: ic.Writer}, AlbumPosStep/2)) // c to the front
	add(a.RemoveItem("alb-1", WriteKey{At: ia.At, Writer: ia.Writer}))

	// Device B, offline from before the remove, re-adds photoA — a different key the
	// remove never observed — and adds the dedupe twin of photoA.
	b := NewAlbums()
	b.Apply(changes[:5]...)
	add(b.AddItem("alb-1", photoA))
	add(b.AddItem("alb-1", photoDup))

	// A second album, created then deleted.
	add(a.Create("alb-2", "Scratch"))
	add(a.AddItem("alb-2", photoC))
	add(a.Delete("alb-2"))

	want = NewAlbums()
	want.Apply(changes...)
	return changes, want
}

func permuteAlbum(src *rand.Rand, changes []AlbumChange) []AlbumChange {
	out := make([]AlbumChange, len(changes))
	copy(out, changes)
	src.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestAlbumConvergesUnderReordering is the headline property (§43): the same set
// of writes folds to a byte-identical set — Encode AND Snapshot — regardless of
// arrival order, including a move or a remove arriving before its add.
func TestAlbumConvergesUnderReordering(t *testing.T) {
	changes, want := buildAlbumChangeset(t)
	wantSnap, _ := want.Snapshot()
	src := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test shuffle
	for i := 0; i < 200; i++ {
		got := NewAlbums()
		got.Apply(permuteAlbum(src, changes)...)
		if got.Encode() != want.Encode() {
			t.Fatalf("order %d diverged:\n got=%s\nwant=%s", i, got.Encode(), want.Encode())
		}
		if snap, _ := got.Snapshot(); string(snap) != string(wantSnap) {
			t.Fatalf("order %d: snapshot diverged", i)
		}
	}
}

// TestAlbumMergeEqualsApply: folding materialised sets (MergeAlbums) and folding
// the change log (Apply) reach the same state.
func TestAlbumMergeEqualsApply(t *testing.T) {
	changes, want := buildAlbumChangeset(t)
	x, y, z := NewAlbums(), NewAlbums(), NewAlbums()
	x.Apply(changes[:4]...)
	y.Apply(changes[4:9]...)
	z.Apply(changes[9:]...)
	if got := MergeAlbums(x, y, z); got.Encode() != want.Encode() {
		t.Fatalf("merge != apply:\n got=%s\nwant=%s", got.Encode(), want.Encode())
	}
	if got := MergeAlbums(z, nil, y, x, z); got.Encode() != want.Encode() {
		t.Fatal("merge is not commutative / idempotent / nil-tolerant")
	}
}

// TestAlbumIsIdempotent: re-applying the whole set changes nothing.
func TestAlbumIsIdempotent(t *testing.T) {
	changes, want := buildAlbumChangeset(t)
	got := NewAlbums()
	got.Apply(changes...)
	got.Apply(changes...)
	if got.Encode() != want.Encode() {
		t.Fatalf("not idempotent:\n got=%s\nwant=%s", got.Encode(), want.Encode())
	}
}

// TestAlbumResolvedView pins the projection the changeset converges to: c moved
// to the front, b next, then the re-added a (add-wins) with its dedupe twin
// collapsed; the cover is b; the deleted album is absent.
func TestAlbumResolvedView(t *testing.T) {
	_, s := buildAlbumChangeset(t)
	all := s.Resolved()
	if len(all) != 1 || all[0].AlbumID != "alb-1" {
		t.Fatalf("want only alb-1 live, got %+v", all)
	}
	ra := all[0]
	if ra.Title != "Trip" || ra.Caption != "the long weekend" || ra.Cover != photoB.Manifest {
		t.Fatalf("registers wrong: %+v", ra)
	}
	var got []string
	for _, it := range ra.Items {
		got = append(got, it.Photo.Manifest)
	}
	want := []string{photoC.Manifest, photoB.Manifest, photoA.Manifest}
	if len(got) != len(want) {
		t.Fatalf("items: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("items: got %v want %v", got, want)
		}
	}
	if _, ok := s.Album("alb-2"); ok {
		t.Fatal("a deleted album must not resolve")
	}
	// The hidden duplicate is still in the write set (the collapse is a projection).
	if n := len(s.albums["alb-1"].items); n != 5 {
		t.Fatalf("want 5 adds in the write set, got %d", n)
	}
}

// TestAlbumAddWinsOverConcurrentRemove: a remove tombstones only the add key it
// observed; a concurrent re-add under another key survives, in any order.
func TestAlbumAddWinsOverConcurrentRemove(t *testing.T) {
	a := NewAlbums()
	cr := must(t)(a.Create("x", ""))
	first := must(t)(a.AddItem("x", photoA))
	rm := must(t)(a.RemoveItem("x", WriteKey{At: first.At, Writer: first.Writer}))
	b := NewAlbums()
	b.Apply(cr, first)
	again := must(t)(b.AddItem("x", photoA))
	all := []AlbumChange{cr, first, rm, again}
	for seed := int64(0); seed < 50; seed++ {
		s := NewAlbums()
		s.Apply(permuteAlbum(rand.New(rand.NewSource(seed)), all)...) //nolint:gosec // deterministic
		ra, ok := s.Album("x")
		if !ok || len(ra.Items) != 1 || ra.Items[0].Key != (WriteKey{At: again.At, Writer: again.Writer}) {
			t.Fatalf("seed %d: add-wins failed: %+v", seed, ra)
		}
	}
}

// TestAlbumCoverFallsBack: a cover naming a manifest no live item carries falls
// back to the first item; clearing the cover does the same.
func TestAlbumCoverFallsBack(t *testing.T) {
	a := NewAlbums()
	must(t)(a.Create("x", ""))
	ia := must(t)(a.AddItem("x", photoA))
	must(t)(a.AddItem("x", photoB))
	must(t)(a.SetCover("x", photoA.Manifest))
	if ra, _ := a.Album("x"); ra.Cover != photoA.Manifest {
		t.Fatalf("cover should be a, got %s", ra.Cover)
	}
	must(t)(a.RemoveItem("x", WriteKey{At: ia.At, Writer: ia.Writer}))
	if ra, _ := a.Album("x"); ra.Cover != photoB.Manifest {
		t.Fatalf("cover should fall back to b, got %s", ra.Cover)
	}
	must(t)(a.SetCover("x", ""))
	if ra, _ := a.Album("x"); ra.Cover != photoB.Manifest {
		t.Fatalf("cleared cover should fall back to b, got %s", ra.Cover)
	}
}

// TestAlbumDeleteCoversEarlierWritesOnly: a delete hides what came before it; a
// re-create afterwards starts clean, and an add that races the delete (greater
// key) is not covered.
func TestAlbumDeleteCoversEarlierWritesOnly(t *testing.T) {
	a := NewAlbums()
	must(t)(a.Create("x", "Old"))
	must(t)(a.AddItem("x", photoA))
	del := must(t)(a.Delete("x"))
	if _, ok := a.Album("x"); ok {
		t.Fatal("deleted album resolved")
	}
	must(t)(a.Create("x", ""))
	ra, ok := a.Album("x")
	if !ok || ra.Title != "" || len(ra.Items) != 0 {
		t.Fatalf("re-created album should be clean, got %+v ok=%v", ra, ok)
	}
	// A concurrent add with a key greater than the delete is NOT covered.
	late := AlbumChange{Op: AlbumOpAddItem, AlbumID: "x", Item: &photoB, Pos: 1, At: del.At, Writer: "zzzz"}
	a.Apply(late)
	if ra, _ := a.Album("x"); len(ra.Items) != 1 || ra.Items[0].Photo.Manifest != photoB.Manifest {
		t.Fatalf("an add racing the delete should survive, got %+v", ra.Items)
	}
}

// TestAlbumMoveBeforeAddConverges: a move for an item whose add has not arrived
// is held and applied when it does, matching add-then-move.
func TestAlbumMoveBeforeAddConverges(t *testing.T) {
	a := NewAlbums()
	cr := must(t)(a.Create("x", ""))
	ia := must(t)(a.AddItem("x", photoA))
	ib := must(t)(a.AddItem("x", photoB))
	mv := must(t)(a.MoveItem("x", WriteKey{At: ib.At, Writer: ib.Writer}, 1))
	fwd := NewAlbums()
	fwd.Apply(cr, ia, ib, mv)
	rev := NewAlbums()
	rev.Apply(mv, ib, ia, cr)
	if fwd.Encode() != rev.Encode() {
		t.Fatalf("move-before-add diverged:\n%s\nvs\n%s", fwd.Encode(), rev.Encode())
	}
	if ra, _ := rev.Album("x"); ra.Items[0].Photo.Manifest != photoB.Manifest {
		t.Fatal("the moved item did not come first")
	}
	// A move whose key sorts BEFORE the add it targets is ignored.
	stale := AlbumChange{Op: AlbumOpMoveItem, AlbumID: "x", Base: WriteKey{At: ia.At, Writer: ia.Writer}, Pos: 0, At: 1, Writer: "0"}
	fwd.Apply(stale)
	if ra, _ := fwd.Album("x"); ra.Items[1].Photo.Manifest != photoA.Manifest || ra.Items[1].Pos != ia.Pos {
		t.Fatalf("a stale move reordered the item: %+v", ra.Items)
	}
}

// TestAlbumSnapshotRoundTrips: a snapshot reconstructs a byte-identical set.
func TestAlbumSnapshotRoundTrips(t *testing.T) {
	_, a := buildAlbumChangeset(t)
	snap, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	got, err := AlbumsFromSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if got.Encode() != a.Encode() {
		t.Fatalf("snapshot lost state:\n got=%s\nwant=%s", got.Encode(), a.Encode())
	}
	again, _ := got.Snapshot()
	if string(again) != string(snap) {
		t.Fatal("snapshot is not canonical across a round trip")
	}
}

// TestAlbumLocalWritesRejectMalformed: a malformed local write fails at the
// source, writes nothing, and does not move the clock.
func TestAlbumLocalWritesRejectMalformed(t *testing.T) {
	a := NewAlbums()
	must(t)(a.Create("x", ""))
	before := a.counter
	bad := []func() (AlbumChange, error){
		func() (AlbumChange, error) {
			return a.AddItem("x", PhotoManifest{Manifest: "nope", PlaintextHash: hexBlob("aa")})
		},
		func() (AlbumChange, error) { return a.AddItem("x", PhotoManifest{Manifest: hexBlob("aa")}) },
		func() (AlbumChange, error) {
			return a.AddItem("x", PhotoManifest{Manifest: hexBlob("aa"), PlaintextHash: hexBlob("bb"), Thumbnail: "sha256:x"})
		},
		func() (AlbumChange, error) { return a.SetCover("x", "blake3:XYZ") },
		func() (AlbumChange, error) { return a.RemoveItem("x", WriteKey{}) },
		func() (AlbumChange, error) { return a.MoveItem("x", WriteKey{}, 3) },
		func() (AlbumChange, error) { return a.Create("", "no id") },
	}
	for i, f := range bad {
		if _, err := f(); err == nil {
			t.Fatalf("case %d: malformed local write accepted", i)
		}
	}
	if a.counter != before {
		t.Fatalf("a rejected write moved the clock %d -> %d", before, a.counter)
	}
	if ra, _ := a.Album("x"); len(ra.Items) != 0 || ra.Cover != "" {
		t.Fatalf("a rejected write entered the state: %+v", ra)
	}
}

// TestAlbumApplySkipsMalformedChanges: malformed peer changes are dropped, not
// applied, and the drop is deterministic so convergence is preserved.
func TestAlbumApplySkipsMalformedChanges(t *testing.T) {
	valid := []AlbumChange{
		{Op: AlbumOpCreate, AlbumID: "x", At: 1, Writer: "w1"},
		{Op: AlbumOpAddItem, AlbumID: "x", Item: &photoA, Pos: 1, At: 2, Writer: "w2"},
	}
	bad := []AlbumChange{
		{Op: AlbumOpAddItem, AlbumID: "x", At: 3, Writer: "w"},                                                      // no item
		{Op: AlbumOpRemoveItem, AlbumID: "x", At: 4, Writer: "w"},                                                   // no base
		{Op: AlbumOpDelete, AlbumID: "x", Item: &photoB, At: 5, Writer: "w"},                                        // delete carries an item
		{Op: AlbumOpSetTitle, AlbumID: "x", Title: "t", Cover: photoA.Manifest, At: 6, Writer: "w"},                 // title carries a cover
		{Op: AlbumOpCreate, AlbumID: "", At: 7, Writer: "w"},                                                        // no album id
		{Op: AlbumOpCreate, AlbumID: "x", At: 8, Writer: ""},                                                        // no writer
		{Op: AlbumOp(42), AlbumID: "x", At: 9, Writer: "w"},                                                         // unknown op
		{Op: AlbumOpAddItem, AlbumID: "x", Item: &photoB, Base: WriteKey{At: 1, Writer: "w1"}, At: 10, Writer: "w"}, // add carries a base
	}
	a := NewAlbums()
	rejected := a.ApplyReport(append(append([]AlbumChange{}, valid...), bad...)...)
	if len(rejected) != len(bad) {
		t.Fatalf("want %d rejected, got %d", len(bad), len(rejected))
	}
	if ra, ok := a.Album("x"); !ok || len(ra.Items) != 1 || ra.Title != "" {
		t.Fatalf("only the valid changes should survive, got %+v", ra)
	}
	if a.counter != 2 {
		t.Fatalf("a rejected change moved the clock to %d", a.counter)
	}
	all := append(append([]AlbumChange{}, valid...), bad...)
	src := rand.New(rand.NewSource(7)) //nolint:gosec // deterministic test shuffle
	for i := 0; i < 50; i++ {
		got := NewAlbums()
		got.Apply(permuteAlbum(src, all)...)
		if got.Encode() != a.Encode() {
			t.Fatalf("order %d diverged with malformed changes in the mix", i)
		}
	}
}

// --- adversarial / synthetic -------------------------------------------------
//
// Hostile AlbumChange values an authorised-but-malicious device could craft
// (§42): colliding keys with different payloads, registers tied on (At, Writer),
// removes and moves of items never added, a Lamport counter at the ceiling. Every
// replica must still converge regardless of arrival order (§43).

// TestAlbumConvergesUnderHostileOrdering: a pile of arbitrary writes — adds under
// colliding keys, tied registers, ghost removes and moves — folds to one
// byte-identical set in 200 random orders.
func TestAlbumConvergesUnderHostileOrdering(t *testing.T) {
	t.Parallel()
	var changes []AlbumChange
	photos := []PhotoManifest{photoA, photoB, photoC, photoDup}
	for i := 0; i < 24; i++ {
		p := photos[i%len(photos)]
		changes = append(changes, AlbumChange{
			Op: AlbumOpAddItem, AlbumID: "alb-" + string(rune('a'+i%2)), Item: &p,
			Pos: uint64(i % 5), At: uint64(i % 7), Writer: PosTag(string(rune('a' + i%6))),
		})
	}
	changes = append(changes,
		AlbumChange{Op: AlbumOpCreate, AlbumID: "alb-a", Title: "T1", At: 3, Writer: "x"},
		AlbumChange{Op: AlbumOpCreate, AlbumID: "alb-a", Title: "T2", At: 3, Writer: "x"}, // tied create+title
		AlbumChange{Op: AlbumOpCreate, AlbumID: "alb-b", At: 1, Writer: "b"},
		AlbumChange{Op: AlbumOpDelete, AlbumID: "alb-b", At: 2, Writer: "b"},
		AlbumChange{Op: AlbumOpCreate, AlbumID: "alb-b", At: 2, Writer: "c"}, // re-create tied with the delete's At
		AlbumChange{Op: AlbumOpSetCaption, AlbumID: "alb-a", Caption: "k1", At: 5, Writer: "q"},
		AlbumChange{Op: AlbumOpSetCaption, AlbumID: "alb-a", Caption: "k0", At: 5, Writer: "q"},
		AlbumChange{Op: AlbumOpSetCover, AlbumID: "alb-a", Cover: photoB.Manifest, At: 9, Writer: "q"},
		AlbumChange{Op: AlbumOpRemoveItem, AlbumID: "alb-a", Base: WriteKey{At: 2, Writer: "c"}, At: 8, Writer: "r"},
		AlbumChange{Op: AlbumOpRemoveItem, AlbumID: "alb-a", Base: WriteKey{At: 99, Writer: "ghost"}, At: 8, Writer: "s"},
		AlbumChange{Op: AlbumOpMoveItem, AlbumID: "alb-a", Base: WriteKey{At: 4, Writer: "e"}, Pos: 0, At: 9, Writer: "m1"},
		AlbumChange{Op: AlbumOpMoveItem, AlbumID: "alb-a", Base: WriteKey{At: 4, Writer: "e"}, Pos: 7, At: 9, Writer: "m1"}, // tied move
		AlbumChange{Op: AlbumOpMoveItem, AlbumID: "alb-a", Base: WriteKey{At: 77, Writer: "ghost"}, Pos: 1, At: 9, Writer: "m2"},
	)
	want := NewAlbums()
	want.Apply(changes...)
	wantSnap, _ := want.Snapshot()
	for seed := int64(0); seed < 200; seed++ {
		got := NewAlbums()
		got.Apply(permuteAlbum(rand.New(rand.NewSource(seed)), changes)...) //nolint:gosec // deterministic
		if got.Encode() != want.Encode() {
			t.Fatalf("seed %d diverged:\n got=%s\nwant=%s", seed, got.Encode(), want.Encode())
		}
		if snap, _ := got.Snapshot(); string(snap) != string(wantSnap) {
			t.Fatalf("seed %d: snapshot diverged", seed)
		}
	}
	// Merge resolves the same hostile set identically to Apply.
	half := len(changes) / 2
	x, y := NewAlbums(), NewAlbums()
	x.Apply(changes[:half]...)
	y.Apply(changes[half:]...)
	if MergeAlbums(y, x).Encode() != want.Encode() {
		t.Fatal("MergeAlbums resolved the hostile set differently from Apply")
	}
}

// TestAlbumRegisterTiesSettleOnValue: a fully tied register key (same At AND
// Writer, which a UUIDv7 never produces) is broken by the value, so both
// replicas settle on the greater string.
func TestAlbumRegisterTiesSettleOnValue(t *testing.T) {
	t.Parallel()
	cr := AlbumChange{Op: AlbumOpCreate, AlbumID: "x", At: 1, Writer: "w"}
	t1 := AlbumChange{Op: AlbumOpSetTitle, AlbumID: "x", Title: "aaa", At: 2, Writer: "w"}
	t2 := AlbumChange{Op: AlbumOpSetTitle, AlbumID: "x", Title: "bbb", At: 2, Writer: "w"}
	for _, order := range [][]AlbumChange{{cr, t1, t2}, {t2, t1, cr}} {
		s := NewAlbums()
		s.Apply(order...)
		if ra, _ := s.Album("x"); ra.Title != "bbb" {
			t.Fatalf("tied title did not settle on the greater value: %q", ra.Title)
		}
	}
}

// TestAlbumPoisonedClockDoesNotWrap: an At=MaxUint64 change poisons the clock;
// the next local write saturates instead of wrapping to 0.
func TestAlbumPoisonedClockDoesNotWrap(t *testing.T) {
	t.Parallel()
	a := NewAlbums()
	a.Apply(AlbumChange{Op: AlbumOpCreate, AlbumID: "x", At: math.MaxUint64, Writer: "p"})
	c := must(t)(a.SetTitle("x", "after"))
	if c.At != math.MaxUint64 {
		t.Fatalf("saturated At = %d, want MaxUint64", c.At)
	}
	// Positions saturate the same way.
	a.Apply(AlbumChange{Op: AlbumOpAddItem, AlbumID: "x", Item: &photoA, Pos: math.MaxUint64, At: math.MaxUint64, Writer: "q"})
	ib := must(t)(a.AddItem("x", photoB))
	if ib.Pos != math.MaxUint64 {
		t.Fatalf("append after a saturated position wrapped to %d", ib.Pos)
	}
}

// TestAlbumZeroChangeIsRejectedDeterministically: a zero-value AlbumChange (no
// writer, no album id) is malformed and skipped on every replica, so it can never
// make two replicas disagree.
func TestAlbumZeroChangeIsRejectedDeterministically(t *testing.T) {
	t.Parallel()
	real := AlbumChange{Op: AlbumOpCreate, AlbumID: "x", At: 1, Writer: "t"}
	a := NewAlbums()
	a.Apply(AlbumChange{}, real)
	b := NewAlbums()
	b.Apply(real, AlbumChange{})
	if a.Encode() != b.Encode() || a.counter != 1 {
		t.Fatalf("a zero change made the set order-dependent or moved the clock:\n%s\nvs\n%s", a.Encode(), b.Encode())
	}
}
