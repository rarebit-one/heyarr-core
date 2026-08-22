package replication_test

import (
	"reflect"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
)

// The diff, as a value (§19, §57, M4-08).
//
// These are the tests that can say things the database-backed ones cannot,
// because they can construct a fabric that does not exist yet: a peer in a
// mode nothing sets, a holdings map that disagrees with itself, a desired set
// larger than any bound. What they assert is that the decision is a function
// of two sets and of nothing else.

const (
	peerA = "peer-a"
	peerB = "peer-b"
	hashA = "blake3:aaaa"
	hashB = "blake3:bbbb"
)

func full(id string) replication.Peer {
	return replication.Peer{ID: id, Mode: replication.ModeFull}
}

// §19: a Full Peer's desired set is the complete canonical blob set. Not a
// subset, not a filtered view — the whole thing.
func TestAFullPeerWantsTheWholeCanonicalSet(t *testing.T) {
	canonical := []string{hashA, hashB}
	got := replication.DesiredBlobSet(full(peerA), canonical)
	if !reflect.DeepEqual(got, canonical) {
		t.Fatalf("desired set = %v, want %v", got, canonical)
	}
}

// The returned set is a copy. A caller that sorted or truncated it must not be
// able to change what the next peer is diffed against — which is the kind of
// aliasing bug that makes a reconciler produce a different answer depending on
// the ORDER the peers happened to be considered in.
func TestTheDesiredSetIsNotAliasedToTheCanonicalSet(t *testing.T) {
	canonical := []string{hashA, hashB}
	got := replication.DesiredBlobSet(full(peerA), canonical)
	got[0] = "mutated"
	if canonical[0] != hashA {
		t.Fatalf("mutating the desired set changed the canonical set: %v", canonical)
	}
}

// Anything that is not a Full Peer gets an empty set while §34 is unbuilt.
// "Everything, but smaller" would be a placement policy invented as a default.
func TestOnlyFullPeersHaveADesiredSet(t *testing.T) {
	for _, mode := range []string{"partial", "cache", "archive", "compute", ""} {
		got := replication.DesiredBlobSet(replication.Peer{ID: peerA, Mode: mode}, []string{hashA})
		if len(got) != 0 {
			t.Errorf("a %q peer wants %v; §34's placement policies are a later milestone", mode, got)
		}
	}
}

// ADR-0020, asserted DIRECTLY rather than inferred from a total.
//
// A linked asset has no blob at all, so it contributes nothing to the
// canonical set the caller assembles — and there is no filter in this package
// that could be removed to change that. The assertion is that the desired set
// is exactly the blobs supplied and contains no entry for an asset that has
// none: a linked asset is unrepresentable here, not excluded here.
func TestALinkedAssetCannotEnterTheDesiredSet(t *testing.T) {
	// One managed asset's blob. The linked asset alongside it in the catalog
	// has no hash to contribute, so what a caller can hand this function is
	// exactly this.
	canonical := []string{hashA}
	desired := replication.DesiredBlobSet(full(peerA), canonical)
	if len(desired) != 1 || desired[0] != hashA {
		t.Fatalf("desired set = %v, want exactly [%s]", desired, hashA)
	}
	// And the diff over it names that blob and only that blob. A gap for a
	// linked asset would have to carry an empty hash, which is the shape this
	// asserts cannot appear.
	for _, gap := range replication.Diff([]replication.Peer{full(peerA)}, canonical, nil) {
		if gap.BlobHash == "" {
			t.Fatalf("a gap with no blob hash: %+v", gap)
		}
		if gap.BlobHash != hashA {
			t.Fatalf("a gap for a blob nobody supplied: %+v", gap)
		}
	}
}

func TestDiff(t *testing.T) {
	tests := []struct {
		name      string
		peers     []replication.Peer
		canonical []string
		held      replication.Holdings
		want      []replication.Gap
	}{
		{
			name:      "a blob on one peer is work for the other only",
			peers:     []replication.Peer{full(peerA), full(peerB)},
			canonical: []string{hashA},
			held:      replication.Holdings{peerA: {hashA: {}}},
			want:      []replication.Gap{{BlobHash: hashA, PeerID: peerB}},
		},
		{
			name:      "a blob on neither peer is work for both",
			peers:     []replication.Peer{full(peerA), full(peerB)},
			canonical: []string{hashA},
			held:      nil,
			want: []replication.Gap{
				{BlobHash: hashA, PeerID: peerA},
				{BlobHash: hashA, PeerID: peerB},
			},
		},
		{
			name:      "a converged fabric is no work at all",
			peers:     []replication.Peer{full(peerA), full(peerB)},
			canonical: []string{hashA, hashB},
			held: replication.Holdings{
				peerA: {hashA: {}, hashB: {}},
				peerB: {hashA: {}, hashB: {}},
			},
			want: nil,
		},
		{
			name:      "a peer holding a blob nobody wants produces nothing",
			peers:     []replication.Peer{full(peerA)},
			canonical: nil,
			held:      replication.Holdings{peerA: {hashB: {}}},
			want:      nil,
		},
		{
			name:      "a peer that is not full is never a destination",
			peers:     []replication.Peer{full(peerA), {ID: peerB, Mode: "cache"}},
			canonical: []string{hashA},
			held:      replication.Holdings{peerA: {hashA: {}}},
			want:      nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := replication.Diff(tc.peers, tc.canonical, tc.held)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("gaps = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Determinism is not tidiness here: Bound takes a PREFIX of this slice, and a
// prefix of a randomly ordered list would replicate a different arbitrary
// subset every cycle instead of finishing what it started.
func TestDiffIsOrderedRegardlessOfHowThePeersArrive(t *testing.T) {
	canonical := []string{hashB, hashA}
	forwards := replication.Diff([]replication.Peer{full(peerA), full(peerB)}, canonical, nil)
	backwards := replication.Diff([]replication.Peer{full(peerB), full(peerA)}, canonical, nil)
	if !reflect.DeepEqual(forwards, backwards) {
		t.Fatalf("the peer order changed the answer:\n %+v\n %+v", forwards, backwards)
	}
	want := []replication.Gap{
		{BlobHash: hashA, PeerID: peerA},
		{BlobHash: hashA, PeerID: peerB},
		{BlobHash: hashB, PeerID: peerA},
		{BlobHash: hashB, PeerID: peerB},
	}
	if !reflect.DeepEqual(forwards, want) {
		t.Fatalf("gaps = %+v, want %+v", forwards, want)
	}
}

// The dedupe key is blob + DESTINATION, and the destination alone is what
// separates two jobs for the same bytes.
func TestTheDedupeKeyIsTheBlobAndTheDestination(t *testing.T) {
	a := replication.Gap{BlobHash: hashA, PeerID: peerA}
	b := replication.Gap{BlobHash: hashA, PeerID: peerB}
	c := replication.Gap{BlobHash: hashB, PeerID: peerA}
	if a.DedupeKey() == b.DedupeKey() {
		t.Fatal("the same blob to two peers shares a key; one of the two transfers would never be queued")
	}
	if a.DedupeKey() == c.DedupeKey() {
		t.Fatal("two blobs to the same peer share a key")
	}
	if a.DedupeKey() != (replication.Gap{BlobHash: hashA, PeerID: peerA}).DedupeKey() {
		t.Fatal("the key is not stable for the same gap; a second cycle would not dedupe")
	}
}

func TestBound(t *testing.T) {
	gaps := []replication.Gap{
		{BlobHash: hashA, PeerID: peerA},
		{BlobHash: hashA, PeerID: peerB},
		{BlobHash: hashB, PeerID: peerA},
	}
	tests := []struct {
		name         string
		limit        int
		wantTaken    int
		wantDeferred int
	}{
		{"under the bound takes everything", 10, 3, 0},
		{"exactly at the bound defers nothing", 3, 3, 0},
		{"over the bound defers the remainder", 2, 2, 1},
		{"no bound takes everything", 0, 3, 0},
		{"a negative bound is no bound", -1, 3, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			taken, deferred := replication.Bound(gaps, tc.limit)
			if len(taken) != tc.wantTaken {
				t.Errorf("taken = %d, want %d", len(taken), tc.wantTaken)
			}
			if deferred != tc.wantDeferred {
				t.Errorf("deferred = %d, want %d", deferred, tc.wantDeferred)
			}
			// The deferred count must ACCOUNT for the remainder rather than
			// merely be non-zero: a bound that dropped work silently and
			// reported a plausible number would pass a weaker assertion.
			if len(taken)+deferred != len(gaps) {
				t.Errorf("%d taken + %d deferred = %d, but there were %d gaps: work was dropped",
					len(taken), deferred, len(taken)+deferred, len(gaps))
			}
		})
	}
}

// The scoped key must never collide with the fabric-wide one, or an operator
// asking about one peer would silently get back the running whole-fabric cycle.
func TestTheScopedCycleKeyIsDistinct(t *testing.T) {
	if replication.ScopedReconcilePeerDedupeKey(peerA) == replication.ReconcilePeerDedupeKey {
		t.Fatal("a scoped cycle shares the fabric-wide key")
	}
	if replication.ScopedReconcilePeerDedupeKey(peerA) == replication.ScopedReconcilePeerDedupeKey(peerB) {
		t.Fatal("two peers share a scoped key")
	}
	if replication.ScopedReconcilePeerDedupeKey("") != replication.ReconcilePeerDedupeKey {
		t.Fatal("an empty scope must be the fabric-wide cycle")
	}
}
