package crdt

import (
	"math/rand"
	"reflect"
	"testing"
)

// buildStarChangeset returns a deterministic set of star/unstar changes with real
// concurrency baked in — a concurrent star and an unstar of the same item, an
// unstar that observes an earlier star, plain stars — together with the ids
// expected present after they converge, most-recent first. Tags are minted once,
// so every permutation folds the SAME changes; only their order varies.
func buildStarChangeset(t *testing.T) (changes []StarChange, wantIDs []string) {
	t.Helper()

	dev1 := NewStarSet()
	starA := dev1.Star("a")
	starB := dev1.Star("b")
	starC := dev1.Star("c")

	dev2 := NewStarSet()
	starBConcurrent := dev2.Star("b")
	starD := dev2.Star("d")

	// dev1 unstars b, observing only the tag it knows (starB). The concurrent
	// star of b on dev2 is unobserved, so add-wins keeps b starred.
	unstarB := StarChange{Op: OpUnstar, ItemID: "b", Observed: []StarTag{starB.Tag}}

	changes = []StarChange{starA, starB, starC, starBConcurrent, starD, unstarB}

	converged := NewStarSet()
	converged.Apply(changes...)
	wantIDs = converged.StarredIDs()
	return changes, wantIDs
}

func permuteStars(src *rand.Rand, changes []StarChange) []StarChange {
	out := make([]StarChange, len(changes))
	copy(out, changes)
	src.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestStarredConvergesUnderReordering is the headline property (§43): the same
// changes, folded in ANY order and with duplicates, converge to a byte-identical
// set and the same recency order. This is the test the SABOTAGE NOTE in Starred
// refers to.
func TestStarredConvergesUnderReordering(t *testing.T) {
	t.Parallel()
	changes, wantIDs := buildStarChangeset(t)

	baseline := NewStarSet()
	baseline.Apply(changes...)
	want := baseline.Encode()

	src := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test shuffle, not crypto
	for i := 0; i < 500; i++ {
		perm := permuteStars(src, changes)
		if i%3 == 0 && len(perm) > 0 {
			perm = append(perm, perm[i%len(perm)])
		}
		s := NewStarSet()
		s.Apply(perm...)
		if got := s.Encode(); got != want {
			t.Fatalf("permutation %d did not converge:\n got=%s\nwant=%s", i, got, want)
		}
		if got := s.StarredIDs(); !reflect.DeepEqual(got, wantIDs) {
			t.Fatalf("permutation %d order diverged: got=%v want=%v", i, got, wantIDs)
		}
	}
}

func TestStarredIdempotence(t *testing.T) {
	t.Parallel()
	changes, _ := buildStarChangeset(t)
	once := NewStarSet()
	once.Apply(changes...)
	twice := NewStarSet()
	twice.Apply(changes...)
	twice.Apply(changes...)
	if once.Encode() != twice.Encode() {
		t.Fatalf("applying twice differs from once:\n once=%s\ntwice=%s", once.Encode(), twice.Encode())
	}
}

func TestStarredCommutativity(t *testing.T) {
	t.Parallel()
	changes, _ := buildStarChangeset(t)
	a, b := NewStarSet(), NewStarSet()
	for i, c := range changes {
		if i%2 == 0 {
			a.Apply(c)
		} else {
			b.Apply(c)
		}
	}
	if MergeStars(a, b).Encode() != MergeStars(b, a).Encode() {
		t.Fatal("MergeStars is not commutative")
	}
}

func TestStarredAssociativity(t *testing.T) {
	t.Parallel()
	changes, _ := buildStarChangeset(t)
	a, b, c := NewStarSet(), NewStarSet(), NewStarSet()
	for i, ch := range changes {
		switch i % 3 {
		case 0:
			a.Apply(ch)
		case 1:
			b.Apply(ch)
		default:
			c.Apply(ch)
		}
	}
	if MergeStars(MergeStars(a, b), c).Encode() != MergeStars(a, MergeStars(b, c)).Encode() {
		t.Fatal("MergeStars is not associative")
	}
}

// TestStarAddWins: a concurrent star and unstar of the same item leaves it
// STARRED, because the unstar tombstones only the tags it observed.
func TestStarAddWins(t *testing.T) {
	t.Parallel()
	dev1 := NewStarSet()
	star1 := dev1.Star("song")
	unstar := dev1.Unstar("song")

	dev2 := NewStarSet()
	star2 := dev2.Star("song")

	if len(unstar.Observed) != 1 || unstar.Observed[0] != star1.Tag {
		t.Fatalf("unstar observed %v, want exactly [%s]", unstar.Observed, star1.Tag)
	}
	for _, order := range [][]StarChange{
		{star1, unstar, star2},
		{star2, star1, unstar},
		{unstar, star2, star1},
	} {
		s := NewStarSet()
		s.Apply(order...)
		if !s.IsStarred("song") {
			t.Fatalf("add-wins failed for order %v: song not starred", order)
		}
	}
}

// TestUnstarWithoutConcurrentStarIsGone: an unstar that observes every live tag
// removes the star, so add-wins is not trivially satisfied.
func TestUnstarWithoutConcurrentStarIsGone(t *testing.T) {
	t.Parallel()
	s := NewStarSet()
	s.Star("a")
	s.Star("b")
	s.Unstar("a")
	if got := s.StarredIDs(); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("after unstarring a, starred=%v, want [b]", got)
	}
}

// TestStarRecencyOrder: getStarred2 lists most-recently-starred first, and the
// order follows the At counter, not application order.
func TestStarRecencyOrder(t *testing.T) {
	t.Parallel()
	s := NewStarSet()
	first := s.Star("first")   // At 1
	second := s.Star("second") // At 2
	third := s.Star("third")   // At 3

	reordered := NewStarSet()
	reordered.Apply(third, first, second)
	if got := reordered.StarredIDs(); !reflect.DeepEqual(got, []string{"third", "second", "first"}) {
		t.Fatalf("recency order leaked application order: got %v", got)
	}
}

// TestStarLamportAdvancesOnMerge: a device that merges in higher-At stars and
// then stars locally sorts its new star as the most recent.
func TestStarLamportAdvancesOnMerge(t *testing.T) {
	t.Parallel()
	remote := NewStarSet()
	remote.Star("r1")
	remote.Star("r2")
	remoteLast := remote.Star("r3")

	local := NewStarSet()
	local.Apply(remote.changesForTest()...)
	localStar := local.Star("local")

	if localStar.At <= remoteLast.At {
		t.Fatalf("local star At=%d did not advance past merged-in max At=%d", localStar.At, remoteLast.At)
	}
	if got := local.StarredIDs(); got[0] != "local" {
		t.Fatalf("local star did not sort most-recent: %v", got)
	}
}

// changesForTest reconstructs the star changes from live adds — a test-only
// helper standing in for the decrypt-and-ship path.
func (s *StarSet) changesForTest() []StarChange {
	var out []StarChange
	for tag, rec := range s.adds {
		out = append(out, StarChange{Op: OpStar, ItemID: rec.itemID, Tag: tag, At: rec.at})
	}
	return out
}

func TestStarredEmptyStates(t *testing.T) {
	t.Parallel()
	if got := NewStarSet().StarredIDs(); len(got) != 0 {
		t.Fatalf("empty set has stars: %v", got)
	}
	if got := MergeStars().StarredIDs(); len(got) != 0 {
		t.Fatalf("MergeStars() of nothing has stars: %v", got)
	}
	if got := MergeStars(nil, NewStarSet(), nil).StarredIDs(); len(got) != 0 {
		t.Fatalf("MergeStars with nils has stars: %v", got)
	}
}

func TestStarredSnapshotRoundTrips(t *testing.T) {
	t.Parallel()
	s := NewStarSet()
	s.Apply(s.Star("a"), s.Star("b"))
	s.Apply(s.Unstar("a"))
	s.Apply(s.Star("c"))

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	back, err := StarSetFromSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if s.Encode() != back.Encode() {
		t.Fatalf("snapshot did not round-trip:\n%s\n---\n%s", s.Encode(), back.Encode())
	}
}

func TestStarredSnapshotPlusTailEqualsFullReplay(t *testing.T) {
	t.Parallel()
	full := NewStarSet()
	c1 := full.Star("x")
	c2 := full.Star("y")
	snapAt := NewStarSet()
	snapAt.Apply(c1, c2)
	snap, err := snapAt.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c3 := full.Star("z")
	c4 := full.Unstar("x")
	full.Apply(c3, c4)

	fresh, err := StarSetFromSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Apply(c3, c4)
	if full.Encode() != fresh.Encode() {
		t.Fatalf("snapshot+tail != full replay:\nfull=%v\nfresh=%v", full.StarredIDs(), fresh.StarredIDs())
	}
}

func TestConvergedStarSetsSnapshotIdentically(t *testing.T) {
	t.Parallel()
	changes, _ := buildStarChangeset(t)
	a := NewStarSet()
	a.Apply(changes...)
	b := NewStarSet()
	b.Apply(permuteStars(rand.New(rand.NewSource(7)), changes)...) //nolint:gosec // deterministic test shuffle
	sa, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if string(sa) != string(sb) {
		t.Fatalf("converged sets snapshot differently:\n%s\n---\n%s", sa, sb)
	}
}
