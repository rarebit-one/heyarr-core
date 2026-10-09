package catalog_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// gapPairs renders a plan's gaps as "blob@peer" strings, sorted, so a test can
// compare the whole set as a value rather than by index.
func gapPairs(gaps []replication.Gap) []string {
	out := make([]string, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, g.BlobHash+"@"+g.PeerID)
	}
	sort.Strings(out)
	return out
}

// TestConvergenceUnionsPlacementPins is ADR-0096's replication half: a vault blob
// has no `assets` row, so the canonical-set diff never names it, and it reaches a
// peer ONLY because a placement pin says it should. The pin is unioned onto the
// diff per-(blob, peer): a pin to one peer produces a gap for that peer and for
// no other.
func TestConvergenceUnionsPlacementPins(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	// Two Full Peers: this node (created lazily by SelfPeer) and a remote one
	// (seedRemotePeer enrols `remotePeer` and returns this node's id).
	self := h.seedRemotePeer(t)

	// A vault blob: bytes with a blob row but NO asset, so the canonical set does
	// not contain it and the diff alone would produce nothing.
	blob := hashOf('a')
	h.seedBlobs(t, blob)

	// With no pin, convergence finds no gap for it — the proof that the union, not
	// the diff, is what carries a vault blob.
	plan, err := h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatalf("PlanPeerConvergence: %v", err)
	}
	if len(plan.Gaps) != 0 {
		t.Fatalf("a vault blob with no pin should produce no gaps, got %v", gapPairs(plan.Gaps))
	}

	// Pin it to the remote peer only.
	if err := h.cat.PinPlacement(ctx, blob, remotePeer); err != nil {
		t.Fatalf("PinPlacement: %v", err)
	}

	plan, err = h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatalf("PlanPeerConvergence: %v", err)
	}
	// Exactly one gap, and it names the pinned peer. NOT the self peer: a pin to
	// one peer must not fan the blob to every Full Peer, which is the thing
	// ADR-0096 forbids and unioning into the flat canonical set would do.
	if got, want := gapPairs(plan.Gaps), []string{blob + "@" + remotePeer}; !equalStrings(got, want) {
		t.Fatalf("pinned-blob gaps = %v, want %v (self is %s)", got, want, self)
	}
}

// TestConvergencePinToNonFullPeerIsIgnored: §34's placement policies are unbuilt,
// so a pin naming a peer that is not a Full Peer produces no gap — there is no
// policy that says a partial or cache peer should hold anything.
func TestConvergencePinToNonFullPeerIsIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.SelfPeer(ctx); err != nil {
		t.Fatal(err)
	}

	// A cache peer — enrolled, but not `full`.
	h.exec(t, `INSERT INTO peers (id, name, site, mode, is_self, created_at, enrolled_at)
		VALUES ('cache-peer', 'cache', 'site-c', 'cache', 0, ?, ?)`, stamp, stamp)

	blob := hashOf('b')
	h.seedBlobs(t, blob)
	if err := h.cat.PinPlacement(ctx, blob, "cache-peer"); err != nil {
		t.Fatalf("PinPlacement: %v", err)
	}

	plan, err := h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatalf("PlanPeerConvergence: %v", err)
	}
	if len(plan.Gaps) != 0 {
		t.Fatalf("a pin to a non-Full peer should produce no gap, got %v", gapPairs(plan.Gaps))
	}
}

// TestConvergencePinSatisfiedByPresentReplica: a pin whose peer already holds the
// blob is not a gap. The pin says where the blob belongs; a `present` replica says
// it is already there, and convergence has nothing to do.
func TestConvergencePinSatisfiedByPresentReplica(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	h.seedRemotePeer(t)

	blob := hashOf('c')
	h.seedBlobs(t, blob)
	if err := h.cat.PinPlacement(ctx, blob, remotePeer); err != nil {
		t.Fatalf("PinPlacement: %v", err)
	}
	// The remote peer already holds the pinned bytes.
	h.exec(t, `INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
		VALUES (?, ?, 'present', 1024, ?)`, blob, remotePeer, stamp)

	plan, err := h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatalf("PlanPeerConvergence: %v", err)
	}
	if len(plan.Gaps) != 0 {
		t.Fatalf("a pin the peer already satisfies should produce no gap, got %v", gapPairs(plan.Gaps))
	}
}

// TestConvergencePinRespectsScope: a cycle scoped to one peer produces pin gaps
// only for that peer, exactly as the canonical-set diff is scoped — a pin to a
// peer outside the scope is left for that peer's own cycle.
func TestConvergencePinRespectsScope(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)

	blob := hashOf('d')
	h.seedBlobs(t, blob)
	if err := h.cat.PinPlacement(ctx, blob, remotePeer); err != nil {
		t.Fatalf("PinPlacement: %v", err)
	}

	// Scoped to self: the pin names the remote peer, so this cycle sees no gap.
	plan, err := h.cat.PlanPeerConvergence(ctx, self)
	if err != nil {
		t.Fatalf("PlanPeerConvergence(self): %v", err)
	}
	if len(plan.Gaps) != 0 {
		t.Fatalf("a cycle scoped to self should not act on a pin to the remote peer, got %v", gapPairs(plan.Gaps))
	}

	// Scoped to the remote peer: the pin is in scope and produces its gap.
	plan, err = h.cat.PlanPeerConvergence(ctx, remotePeer)
	if err != nil {
		t.Fatalf("PlanPeerConvergence(remote): %v", err)
	}
	if got, want := gapPairs(plan.Gaps), []string{blob + "@" + remotePeer}; !equalStrings(got, want) {
		t.Fatalf("scoped-to-remote gaps = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// catWithStore returns a Catalog backed by the same database and events as h,
// but with the given content store injected. Both catalogs share the peer rows
// seeded via h, because they point to the same database.
func catWithStore(t *testing.T, h *harness, store *cas.FS) *catalog.Catalog {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{
		Writer: h.db.Writer(), Reader: h.db.Reader(),
		Clock: fixedClock{t: ts},
	})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: h.db, Events: eventLog, PeerName: "test", PeerSite: "test-site",
		LocalStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

// TestASelfPinForAnUnknownUnheldBlobIsSkipped: when the planner has a local
// store and a self-pin names a blob with no blobs row whose bytes are ALSO not
// held, no gap is planned (#720). The loop was: plan job → job fails permanently
// with "unknown blob, not held" → next cycle plans again. The result is counted
// as Missing, and the pin is not discarded.
//
// Contrast with the ORIGINAL behaviour (no store injected): without the check
// the planner still produced the gap, as it did in v0.5.4.
func TestASelfPinForAnUnknownUnheldBlobIsSkipped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)

	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := catWithStore(t, h, store)

	// hashOf('0') and hashOf('1') are valid hex hashes with NO blobs rows and
	// NO bytes in the store — the reclaimed case (#720).
	unheld1 := hashOf('0')
	unheld2 := hashOf('1')
	for _, hash := range []string{unheld1, unheld2} {
		if err := h.cat.PinPlacement(ctx, hash, self); err != nil {
			t.Fatalf("PinPlacement(%s): %v", hash, err)
		}
	}

	// Without a local store the old behaviour is preserved: both are planned.
	// (The loop that produced 2,900 dead jobs an hour.)
	planNoStore, err := h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	wantBoth := []string{unheld1 + "@" + self, unheld2 + "@" + self}
	if got := gapPairs(planNoStore.Gaps); !equalStrings(got, wantBoth) {
		t.Fatalf("without store: gaps = %v, want %v", got, wantBoth)
	}
	if planNoStore.Missing != 0 {
		t.Fatalf("without store: Missing = %d, want 0", planNoStore.Missing)
	}

	// With a local store: both unheld self-pins are skipped and counted.
	planWithStore, err := cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if planWithStore.Missing != 2 {
		t.Fatalf("with store: Missing = %d, want 2", planWithStore.Missing)
	}
	for _, g := range planWithStore.Gaps {
		if g.PeerID == self {
			t.Fatalf("with store: gap remains for an unheld self-pin: %v", gapPairs(planWithStore.Gaps))
		}
	}
}

// TestASelfPinForAnUnknownButHeldBlobIsStillPlanned: the adoption path from
// #658 must survive the #720 fix. When bytes ARE in the local store but there
// is no blobs row, the gap is still produced so the handler's held-blob branch
// can run and adopt them.
func TestASelfPinForAnUnknownButHeldBlobIsStillPlanned(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)

	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := catWithStore(t, h, store)

	// Put real bytes into the store — the hash is what the store produces.
	// No blobs row for it: this is the pre-#658 upload state.
	desc, err := store.Put(ctx, strings.NewReader("vault bytes whose row was never written"))
	if err != nil {
		t.Fatal(err)
	}
	held := desc.Hash.String()

	if err := h.cat.PinPlacement(ctx, held, self); err != nil {
		t.Fatalf("PinPlacement: %v", err)
	}

	plan, err := cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{held + "@" + self}
	if got := gapPairs(plan.Gaps); !equalStrings(got, want) {
		t.Fatalf("held-but-unrecorded blob not planned: gaps = %v, want %v", got, want)
	}
	if plan.Missing != 0 {
		t.Fatalf("Missing = %d for a held blob, want 0", plan.Missing)
	}
}
