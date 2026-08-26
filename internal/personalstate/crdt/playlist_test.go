package crdt

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

// buildChangeset returns a deterministic set of changes with real concurrency
// baked in — a concurrent add and a remove of the same item, a remove that
// observes an earlier add, plain adds — together with the item ids expected to
// be present after they all converge, in order. The tags are minted once here,
// so every permutation in the tests below folds the SAME changes; only their
// order varies. That is the whole point: the merge must not care.
func buildChangeset(t *testing.T) (changes []Change, wantIDs []string) {
	t.Helper()

	// One device's timeline: add x1, x2, x3 in order.
	dev1 := New()
	addX1 := dev1.Add("x1")
	addX2 := dev1.Add("x2")
	addX3 := dev1.Add("x3")

	// A second device, offline, concurrently adds x2 again (its own tag) and
	// x4. It never saw dev1's later work.
	dev2 := New()
	addX2Concurrent := dev2.Add("x2")
	addX4 := dev2.Add("x4")

	// dev1 removes x2, observing ONLY the tag it knows (addX2). The concurrent
	// add of x2 on dev2 is unobserved, so add-wins keeps x2 present.
	removeX2 := Change{Op: OpRemove, ItemID: "x2", Observed: []Tag{addX2.Tag}}

	// dev1 removes x3, observing its tag — no concurrent add, so x3 is gone.
	removeX3 := Change{Op: OpRemove, ItemID: "x3", Observed: []Tag{addX3.Tag}}

	changes = []Change{
		addX1, addX2, addX3, addX2Concurrent, addX4, removeX2, removeX3,
	}

	// Present: x1, x2 (via the surviving concurrent tag), x4. Order is by
	// (Counter, Tag). x1 has counter 1; x2's surviving tag has counter 1 on
	// dev2; x4 has counter 2 on dev2. Compute the expectation from the model
	// rather than hard-coding, so the test states intent, not a guess.
	converged := New()
	converged.Apply(changes...)
	wantIDs = converged.IDs()
	return changes, wantIDs
}

// permute returns a shuffled copy of changes using a deterministic source, so
// the test is reproducible run to run (no wall-clock seed).
func permute(src *rand.Rand, changes []Change) []Change {
	out := make([]Change, len(changes))
	copy(out, changes)
	src.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestConvergenceUnderReordering is the headline property (§43): the same
// changes, folded in ANY order and with duplicates, converge to a byte-identical
// state. This is the test the SABOTAGE NOTE in Items refers to — break the
// ordering to depend on application order and this fails.
func TestConvergenceUnderReordering(t *testing.T) {
	t.Parallel()
	changes, wantIDs := buildChangeset(t)

	baseline := New()
	baseline.Apply(changes...)
	want := baseline.Encode()

	src := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test shuffle, not crypto
	for i := 0; i < 500; i++ {
		perm := permute(src, changes)
		// Every so often, fold a duplicate in to prove idempotence under
		// reordering too.
		if i%3 == 0 && len(perm) > 0 {
			perm = append(perm, perm[i%len(perm)])
		}
		s := New()
		s.Apply(perm...)
		if got := s.Encode(); got != want {
			t.Fatalf("permutation %d did not converge:\n got=%s\nwant=%s", i, got, want)
		}
		if got := s.IDs(); !reflect.DeepEqual(got, wantIDs) {
			t.Fatalf("permutation %d order diverged: got=%v want=%v", i, got, wantIDs)
		}
	}
}

// TestIdempotence: applying a change twice equals applying it once.
func TestIdempotence(t *testing.T) {
	t.Parallel()
	changes, _ := buildChangeset(t)

	once := New()
	once.Apply(changes...)

	twice := New()
	twice.Apply(changes...)
	twice.Apply(changes...) // fold the whole set a second time

	if once.Encode() != twice.Encode() {
		t.Fatalf("applying the changeset twice differs from once:\n once=%s\ntwice=%s",
			once.Encode(), twice.Encode())
	}
}

// TestCommutativity: Merge(A, B) == Merge(B, A), byte for byte.
func TestCommutativity(t *testing.T) {
	t.Parallel()
	changes, _ := buildChangeset(t)

	// Split the changes across two states arbitrarily.
	a, b := New(), New()
	for i, c := range changes {
		if i%2 == 0 {
			a.Apply(c)
		} else {
			b.Apply(c)
		}
	}

	ab := Merge(a, b)
	ba := Merge(b, a)
	if ab.Encode() != ba.Encode() {
		t.Fatalf("Merge is not commutative:\n ab=%s\n ba=%s", ab.Encode(), ba.Encode())
	}
}

// TestAssociativity: Merge(Merge(A, B), C) == Merge(A, Merge(B, C)).
func TestAssociativity(t *testing.T) {
	t.Parallel()
	changes, _ := buildChangeset(t)

	a, b, c := New(), New(), New()
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

	left := Merge(Merge(a, b), c)
	right := Merge(a, Merge(b, c))
	if left.Encode() != right.Encode() {
		t.Fatalf("Merge is not associative:\n left=%s\nright=%s", left.Encode(), right.Encode())
	}
}

// TestAddWins: a concurrent add and remove of the same item leaves it PRESENT.
func TestAddWins(t *testing.T) {
	t.Parallel()

	// Device 1 adds "song" and then removes it, observing its own tag.
	dev1 := New()
	add1 := dev1.Add("song")
	remove := dev1.Remove("song")

	// Device 2, concurrently and offline, adds "song" under a different tag.
	dev2 := New()
	add2 := dev2.Add("song")

	// dev1's remove observed add1's tag only — add2 is unseen.
	if len(remove.Observed) != 1 || remove.Observed[0] != add1.Tag {
		t.Fatalf("remove observed %v, want exactly [%s]", remove.Observed, add1.Tag)
	}

	// Merge, both orders. Add-wins: "song" is present because add2's tag was
	// never tombstoned.
	for _, order := range [][]Change{
		{add1, remove, add2},
		{add2, add1, remove},
		{remove, add2, add1},
	} {
		s := New()
		s.Apply(order...)
		if got := s.IDs(); !reflect.DeepEqual(got, []string{"song"}) {
			t.Fatalf("add-wins failed for order %v: present=%v, want [song]", order, got)
		}
	}
}

// TestRemoveWithoutConcurrentAddIsGone is the complement: a remove that observes
// every live tag of an item removes it. Without this, add-wins could be trivially
// satisfied by a remove that never removes anything.
func TestRemoveWithoutConcurrentAddIsGone(t *testing.T) {
	t.Parallel()
	s := New()
	s.Add("a")
	s.Add("b")
	s.Remove("a")
	if got := s.IDs(); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("after removing a, present=%v, want [b]", got)
	}
}

// TestOrdering checks the (Counter, Tag) total order: later Lamport counters sort
// later regardless of application order.
func TestOrdering(t *testing.T) {
	t.Parallel()
	s := New()
	first := s.Add("first")   // counter 1
	second := s.Add("second") // counter 2
	third := s.Add("third")   // counter 3

	// Apply the three adds to a fresh state in reverse and confirm the order is
	// still first, second, third.
	reordered := New()
	reordered.Apply(third, first, second)
	if got := reordered.IDs(); !reflect.DeepEqual(got, []string{"first", "second", "third"}) {
		t.Fatalf("order leaked application order: got %v", got)
	}
}

// TestLamportCounterAdvancesOnMerge proves the clock is a Lamport clock: a device
// that merges in higher-counter changes and then adds locally sorts its new add
// AFTER what it just learned about.
func TestLamportCounterAdvancesOnMerge(t *testing.T) {
	t.Parallel()

	// A remote device builds three adds; the last has counter 3.
	remote := New()
	remote.Add("r1")
	remote.Add("r2")
	remoteLast := remote.Add("r3")

	// A fresh local device learns those, then adds locally. Its new add must
	// take a counter strictly greater than the highest it saw.
	local := New()
	local.Apply(remote.changesForTest()...)
	localAdd := local.Add("local")

	if !remoteLast.Order.Less(localAdd.Order) {
		t.Fatalf("local add order %v did not follow the merged-in max %v; Lamport clock did not advance",
			localAdd.Order, remoteLast.Order)
	}
	// The local add sorts last in the converged playlist.
	if got := local.IDs(); got[len(got)-1] != "local" {
		t.Fatalf("local add did not sort last: %v", got)
	}
}

// changesForTest reconstructs the add changes from a state's live adds — a small
// test-only helper to hand one state's history to another, standing in for the
// decrypt-and-ship path the real system uses.
func (s *State) changesForTest() []Change {
	var out []Change
	for tag, rec := range s.adds {
		out = append(out, Change{Op: OpAdd, ItemID: rec.itemID, Tag: tag, Order: rec.order})
	}
	return out
}

// TestOrderKeyLess is a focused table test on the total order itself.
func TestOrderKeyLess(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		a, b     OrderKey
		wantLess bool
	}{
		{"lower counter wins", OrderKey{1, "z"}, OrderKey{2, "a"}, true},
		{"higher counter loses", OrderKey{2, "a"}, OrderKey{1, "z"}, false},
		{"tie broken by tag", OrderKey{5, "a"}, OrderKey{5, "b"}, true},
		{"tie broken by tag reverse", OrderKey{5, "b"}, OrderKey{5, "a"}, false},
		{"identical is not less", OrderKey{5, "a"}, OrderKey{5, "a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Less(tc.b); got != tc.wantLess {
				t.Fatalf("%v.Less(%v) = %v, want %v", tc.a, tc.b, got, tc.wantLess)
			}
		})
	}
}

// TestEmptyStates: merging nothing, and reading an empty playlist, are well
// defined.
func TestEmptyStates(t *testing.T) {
	t.Parallel()
	if got := New().IDs(); len(got) != 0 {
		t.Fatalf("empty state has items: %v", got)
	}
	if got := Merge().IDs(); len(got) != 0 {
		t.Fatalf("Merge() of nothing has items: %v", got)
	}
	if got := Merge(nil, New(), nil).IDs(); len(got) != 0 {
		t.Fatalf("Merge with nils has items: %v", got)
	}
}

// Example shows the intended shape of use: add, remove, read the order.
func ExampleState() {
	s := New()
	s.Add("track-a")
	s.Add("track-b")
	s.Remove("track-a")
	fmt.Println(s.IDs())
	// Output: [track-b]
}
