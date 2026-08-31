package crdt

import (
	"math/rand"
	"reflect"
	"testing"
)

// buildReadingChangeset returns a deterministic set of position writes with real
// concurrency — two devices writing different positions to the same publication,
// plus independent publications — together with the converged map's expected
// entries. Writers are minted once, so every permutation folds the SAME writes.
func buildReadingChangeset(t *testing.T) (changes []PositionChange, want []PositionEntry) {
	t.Helper()

	dev1 := NewReadingPositions()
	p1a := dev1.Set("book-1", "page-10")
	p1b := dev1.Set("book-2", "page-3") // At 2 on dev1

	// dev2, offline, writes book-1 again (concurrently) and book-3.
	dev2 := NewReadingPositions()
	p2a := dev2.Set("book-1", "page-40") // At 1 on dev2 — concurrent with p1a (At 1)
	p2b := dev2.Set("book-3", "page-7")  // At 2 on dev2

	changes = []PositionChange{p1a, p1b, p2a, p2b}

	converged := NewReadingPositions()
	converged.Apply(changes...)
	want = converged.All()
	return changes, want
}

func permutePositions(src *rand.Rand, changes []PositionChange) []PositionChange {
	out := make([]PositionChange, len(changes))
	copy(out, changes)
	src.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestReadingConvergesUnderReordering is the headline property (§43): the same
// writes, folded in ANY order and with duplicates, converge to a byte-identical
// map. This is the test the SABOTAGE NOTE in All refers to.
func TestReadingConvergesUnderReordering(t *testing.T) {
	t.Parallel()
	changes, want := buildReadingChangeset(t)

	baseline := NewReadingPositions()
	baseline.Apply(changes...)
	wantEnc := baseline.Encode()

	src := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test shuffle
	for i := 0; i < 500; i++ {
		perm := permutePositions(src, changes)
		if i%3 == 0 && len(perm) > 0 {
			perm = append(perm, perm[i%len(perm)])
		}
		r := NewReadingPositions()
		r.Apply(perm...)
		if got := r.Encode(); got != wantEnc {
			t.Fatalf("permutation %d did not converge:\n got=%s\nwant=%s", i, got, wantEnc)
		}
		if got := r.All(); !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %d entries diverged: got=%v want=%v", i, got, want)
		}
	}
}

func TestReadingIdempotence(t *testing.T) {
	t.Parallel()
	changes, _ := buildReadingChangeset(t)
	once := NewReadingPositions()
	once.Apply(changes...)
	twice := NewReadingPositions()
	twice.Apply(changes...)
	twice.Apply(changes...)
	if once.Encode() != twice.Encode() {
		t.Fatalf("applying twice differs from once:\n once=%s\ntwice=%s", once.Encode(), twice.Encode())
	}
}

func TestReadingCommutativity(t *testing.T) {
	t.Parallel()
	changes, _ := buildReadingChangeset(t)
	a, b := NewReadingPositions(), NewReadingPositions()
	for i, c := range changes {
		if i%2 == 0 {
			a.Apply(c)
		} else {
			b.Apply(c)
		}
	}
	if MergeReadingPositions(a, b).Encode() != MergeReadingPositions(b, a).Encode() {
		t.Fatal("MergeReadingPositions is not commutative")
	}
}

func TestReadingAssociativity(t *testing.T) {
	t.Parallel()
	changes, _ := buildReadingChangeset(t)
	a, b, c := NewReadingPositions(), NewReadingPositions(), NewReadingPositions()
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
	if MergeReadingPositions(MergeReadingPositions(a, b), c).Encode() !=
		MergeReadingPositions(a, MergeReadingPositions(b, c)).Encode() {
		t.Fatal("MergeReadingPositions is not associative")
	}
}

// TestReadingLaterWriteWins: a later write (higher At) to the same publication
// overwrites an earlier one, regardless of application order.
func TestReadingLaterWriteWins(t *testing.T) {
	t.Parallel()
	r := NewReadingPositions()
	first := r.Set("book", "page-1")  // At 1
	second := r.Set("book", "page-5") // At 2

	reordered := NewReadingPositions()
	reordered.Apply(second, first) // apply the later one first
	if got, _ := reordered.Position("book"); got != "page-5" {
		t.Fatalf("later write did not win regardless of order: got %q", got)
	}
}

// TestReadingConcurrentWriteResolvesDeterministically: two concurrent writes at
// the SAME At settle on the same position on every replica (by Writer tie-break),
// not by arrival order.
func TestReadingConcurrentWriteResolvesDeterministically(t *testing.T) {
	t.Parallel()
	a := PositionChange{PubID: "b", Position: "posA", At: 1, Writer: "w-aaa"}
	b := PositionChange{PubID: "b", Position: "posB", At: 1, Writer: "w-bbb"}

	ab := NewReadingPositions()
	ab.Apply(a, b)
	ba := NewReadingPositions()
	ba.Apply(b, a)
	if ab.Encode() != ba.Encode() {
		t.Fatalf("concurrent writes diverged by order:\n%s\nvs\n%s", ab.Encode(), ba.Encode())
	}
	// The greater Writer ("w-bbb") wins deterministically.
	if got, _ := ab.Position("b"); got != "posB" {
		t.Fatalf("the Writer tie-break did not settle deterministically: got %q", got)
	}
}

// TestReadingLamportAdvancesOnMerge: a device that merges in higher-At writes and
// then writes locally takes a strictly greater At, so its write wins.
func TestReadingLamportAdvancesOnMerge(t *testing.T) {
	t.Parallel()
	remote := NewReadingPositions()
	remote.Set("x", "p1")
	remote.Set("x", "p2")
	remoteLast := remote.Set("x", "p3") // At 3

	local := NewReadingPositions()
	local.Apply(remote.changesForTest()...)
	localWrite := local.Set("x", "local-pos")

	if localWrite.At <= remoteLast.At {
		t.Fatalf("local write At=%d did not advance past merged-in max At=%d", localWrite.At, remoteLast.At)
	}
	if got, _ := local.Position("x"); got != "local-pos" {
		t.Fatalf("local write did not win after advancing the clock: got %q", got)
	}
}

// changesForTest reconstructs the writes from the map — a test-only helper
// standing in for the decrypt-and-ship path.
func (r *ReadingPositions) changesForTest() []PositionChange {
	var out []PositionChange
	for pub, rec := range r.positions {
		out = append(out, PositionChange{PubID: pub, Position: rec.position, At: rec.key.At, Writer: rec.key.Writer})
	}
	return out
}

func TestReadingEmptyStates(t *testing.T) {
	t.Parallel()
	if got := NewReadingPositions().All(); len(got) != 0 {
		t.Fatalf("empty map has positions: %v", got)
	}
	if _, ok := NewReadingPositions().Position("nope"); ok {
		t.Fatal("empty map reported a position")
	}
	if got := MergeReadingPositions(nil, NewReadingPositions(), nil).All(); len(got) != 0 {
		t.Fatalf("MergeReadingPositions with nils has positions: %v", got)
	}
}

func TestReadingSnapshotRoundTrips(t *testing.T) {
	t.Parallel()
	r := NewReadingPositions()
	r.Apply(r.Set("book-1", "p10"), r.Set("book-2", "p3"))
	r.Apply(r.Set("book-1", "p20")) // overwrite

	snap, err := r.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadingPositionsFromSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if r.Encode() != back.Encode() {
		t.Fatalf("snapshot did not round-trip:\n%s\n---\n%s", r.Encode(), back.Encode())
	}
}

func TestReadingSnapshotPlusTailEqualsFullReplay(t *testing.T) {
	t.Parallel()
	full := NewReadingPositions()
	c1 := full.Set("x", "p1")
	c2 := full.Set("y", "p1")
	snapAt := NewReadingPositions()
	snapAt.Apply(c1, c2)
	snap, err := snapAt.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c3 := full.Set("x", "p2")
	c4 := full.Set("z", "p1")
	full.Apply(c3, c4)

	fresh, err := ReadingPositionsFromSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Apply(c3, c4)
	if full.Encode() != fresh.Encode() {
		t.Fatalf("snapshot+tail != full replay:\n%s\n---\n%s", full.Encode(), fresh.Encode())
	}
}

func TestConvergedReadingMapsSnapshotIdentically(t *testing.T) {
	t.Parallel()
	changes, _ := buildReadingChangeset(t)
	a := NewReadingPositions()
	a.Apply(changes...)
	b := NewReadingPositions()
	b.Apply(permutePositions(rand.New(rand.NewSource(9)), changes)...) //nolint:gosec // deterministic test shuffle
	sa, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if string(sa) != string(sb) {
		t.Fatalf("converged maps snapshot differently:\n%s\n---\n%s", sa, sb)
	}
}
