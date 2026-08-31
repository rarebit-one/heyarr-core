package crdt

import (
	"math/rand"
	"reflect"
	"testing"
)

// buildPlayChangeset returns a deterministic set of play events with real
// concurrency — two devices scrobbling the same and different items offline —
// together with the converged Recent and Frequent views. Tags are minted once,
// so every permutation folds the SAME events.
func buildPlayChangeset(t *testing.T) (changes []PlayChange, wantRecent, wantFrequent []string) {
	t.Helper()

	dev1 := NewPlayLog()
	a1 := dev1.Record("a")
	b1 := dev1.Record("b")
	a2 := dev1.Record("a") // a played twice on dev1

	dev2 := NewPlayLog()
	c1 := dev2.Record("c")
	a3 := dev2.Record("a") // a also played on dev2 — count must sum across devices

	changes = []PlayChange{a1, b1, a2, c1, a3}

	converged := NewPlayLog()
	converged.Apply(changes...)
	wantRecent = idsOf(converged.Recent())
	wantFrequent = idsOf(converged.Frequent())
	return changes, wantRecent, wantFrequent
}

func idsOf(entries []PlayEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func permutePlays(src *rand.Rand, changes []PlayChange) []PlayChange {
	out := make([]PlayChange, len(changes))
	copy(out, changes)
	src.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestPlayConvergesUnderReordering is the headline property (§43): the same
// events, folded in ANY order and with duplicates, converge to a byte-identical
// log and the same Recent/Frequent orders. This is the test the SABOTAGE NOTE in
// Recent refers to.
func TestPlayConvergesUnderReordering(t *testing.T) {
	t.Parallel()
	changes, wantRecent, wantFrequent := buildPlayChangeset(t)

	baseline := NewPlayLog()
	baseline.Apply(changes...)
	want := baseline.Encode()

	src := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test shuffle
	for i := 0; i < 500; i++ {
		perm := permutePlays(src, changes)
		if i%3 == 0 && len(perm) > 0 {
			perm = append(perm, perm[i%len(perm)])
		}
		l := NewPlayLog()
		l.Apply(perm...)
		if got := l.Encode(); got != want {
			t.Fatalf("permutation %d did not converge:\n got=%s\nwant=%s", i, got, want)
		}
		if got := idsOf(l.Recent()); !reflect.DeepEqual(got, wantRecent) {
			t.Fatalf("permutation %d Recent diverged: got=%v want=%v", i, got, wantRecent)
		}
		if got := idsOf(l.Frequent()); !reflect.DeepEqual(got, wantFrequent) {
			t.Fatalf("permutation %d Frequent diverged: got=%v want=%v", i, got, wantFrequent)
		}
	}
}

func TestPlayIdempotence(t *testing.T) {
	t.Parallel()
	changes, _, _ := buildPlayChangeset(t)
	once := NewPlayLog()
	once.Apply(changes...)
	twice := NewPlayLog()
	twice.Apply(changes...)
	twice.Apply(changes...)
	if once.Encode() != twice.Encode() {
		t.Fatalf("applying twice differs from once:\n once=%s\ntwice=%s", once.Encode(), twice.Encode())
	}
}

func TestPlayCommutativity(t *testing.T) {
	t.Parallel()
	changes, _, _ := buildPlayChangeset(t)
	a, b := NewPlayLog(), NewPlayLog()
	for i, c := range changes {
		if i%2 == 0 {
			a.Apply(c)
		} else {
			b.Apply(c)
		}
	}
	if MergePlayLogs(a, b).Encode() != MergePlayLogs(b, a).Encode() {
		t.Fatal("MergePlayLogs is not commutative")
	}
}

func TestPlayAssociativity(t *testing.T) {
	t.Parallel()
	changes, _, _ := buildPlayChangeset(t)
	a, b, c := NewPlayLog(), NewPlayLog(), NewPlayLog()
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
	if MergePlayLogs(MergePlayLogs(a, b), c).Encode() != MergePlayLogs(a, MergePlayLogs(b, c)).Encode() {
		t.Fatal("MergePlayLogs is not associative")
	}
}

// TestPlayCountSumsAcrossDevices: a play count is the number of events, so two
// devices' concurrent scrobbles of the same item SUM rather than one overwriting
// the other (the failure a naive per-item LWW counter would have).
func TestPlayCountSumsAcrossDevices(t *testing.T) {
	t.Parallel()
	dev1 := NewPlayLog()
	x1 := dev1.Record("x")
	x2 := dev1.Record("x")

	dev2 := NewPlayLog()
	x3 := dev2.Record("x") // concurrent, offline

	merged := NewPlayLog()
	merged.Apply(x1, x2, x3)
	if got := merged.Count("x"); got != 3 {
		t.Fatalf("play count did not sum across devices: got %d, want 3", got)
	}
}

// TestNowPlayingIsMostRecent: getNowPlaying returns the item of the greatest
// (At, Tag) event, regardless of application order.
func TestNowPlayingIsMostRecent(t *testing.T) {
	t.Parallel()
	l := NewPlayLog()
	l.Record("first")
	l.Record("second")
	last := l.Record("third")

	reordered := NewPlayLog()
	reordered.Apply(l.changesForTest()...)
	if item, ok := reordered.NowPlaying(); !ok || item != "third" {
		t.Fatalf("now-playing = %q (ok=%v), want third", item, ok)
	}
	// And it matches the greatest event's item.
	if last.ItemID != "third" {
		t.Fatalf("test setup: last recorded %q, want third", last.ItemID)
	}
	// Empty log has no now-playing.
	if _, ok := NewPlayLog().NowPlaying(); ok {
		t.Fatal("empty log reported a now-playing item")
	}
}

// TestFrequentRanksByCount: frequent orders items by play count, breaking ties by
// most-recent then id.
func TestFrequentRanksByCount(t *testing.T) {
	t.Parallel()
	l := NewPlayLog()
	l.Record("rare")
	l.Record("common")
	l.Record("common")
	l.Record("common")
	l.Record("mid")
	l.Record("mid")

	if got := idsOf(l.Frequent()); !reflect.DeepEqual(got, []string{"common", "mid", "rare"}) {
		t.Fatalf("frequent order = %v, want [common mid rare]", got)
	}
}

// changesForTest reconstructs the play events from the log — a test-only helper
// standing in for the decrypt-and-ship path.
func (l *PlayLog) changesForTest() []PlayChange {
	var out []PlayChange
	for tag, rec := range l.events {
		out = append(out, PlayChange{ItemID: rec.itemID, Tag: tag, At: rec.at})
	}
	return out
}

func TestPlayEmptyStates(t *testing.T) {
	t.Parallel()
	if got := NewPlayLog().Recent(); len(got) != 0 {
		t.Fatalf("empty log has recent items: %v", got)
	}
	if got := NewPlayLog().Count("nope"); got != 0 {
		t.Fatalf("empty log has a non-zero count: %d", got)
	}
	if got := MergePlayLogs(nil, NewPlayLog(), nil).Recent(); len(got) != 0 {
		t.Fatalf("MergePlayLogs with nils has items: %v", got)
	}
}

func TestPlaySnapshotRoundTrips(t *testing.T) {
	t.Parallel()
	l := NewPlayLog()
	l.Apply(l.Record("a"), l.Record("b"), l.Record("a"))

	snap, err := l.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	back, err := PlayLogFromSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if l.Encode() != back.Encode() {
		t.Fatalf("snapshot did not round-trip:\n%s\n---\n%s", l.Encode(), back.Encode())
	}
}

func TestPlaySnapshotPlusTailEqualsFullReplay(t *testing.T) {
	t.Parallel()
	full := NewPlayLog()
	c1 := full.Record("x")
	c2 := full.Record("y")
	snapAt := NewPlayLog()
	snapAt.Apply(c1, c2)
	snap, err := snapAt.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c3 := full.Record("z")
	c4 := full.Record("x")
	full.Apply(c3, c4)

	fresh, err := PlayLogFromSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Apply(c3, c4)
	if full.Encode() != fresh.Encode() {
		t.Fatalf("snapshot+tail != full replay:\n%s\n---\n%s", full.Encode(), fresh.Encode())
	}
}

func TestConvergedPlayLogsSnapshotIdentically(t *testing.T) {
	t.Parallel()
	changes, _, _ := buildPlayChangeset(t)
	a := NewPlayLog()
	a.Apply(changes...)
	b := NewPlayLog()
	b.Apply(permutePlays(rand.New(rand.NewSource(11)), changes)...) //nolint:gosec // deterministic test shuffle
	sa, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if string(sa) != string(sb) {
		t.Fatalf("converged logs snapshot differently:\n%s\n---\n%s", sa, sb)
	}
}
