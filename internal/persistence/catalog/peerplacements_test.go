package catalog_test

import (
	"context"
	"testing"

	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/placementsync"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// all is the whole hash space as one range.
var all = placementsync.Range{}

// holdHere records a present replica of blob on peer.
func (h *harness) holdOn(t *testing.T, blob, peer string) {
	t.Helper()
	h.exec(t, `INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
		VALUES (?, ?, 'present', 1024, ?)`, blob, peer, stamp)
}

func hashesOf(es []placementsync.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.BlobHash
	}
	return out
}

// TestOfferPlacements is the source's half (ADR-0114): what this node offers an
// asking peer. A device pin naming the asker is offered to any asker; an
// upload's self-pin only to a configured sibling; a pinned blob this node lost
// is still offered, marked not held; and never a placement this node took from
// someone else.
func TestOfferPlacements(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)

	devicePinned := hashOf('a') // pinned by a device to the remote peer
	uploaded := hashOf('b')     // an upload here: pinned to self
	notHeld := hashOf('c')      // pinned to self, but the bytes are not here
	h.seedBlobs(t, devicePinned, uploaded, notHeld)
	h.holdOn(t, devicePinned, self)
	h.holdOn(t, uploaded, self)
	for _, pin := range [][2]string{{devicePinned, remotePeer}, {uploaded, self}, {notHeld, self}} {
		if err := h.cat.PinPlacement(ctx, pin[0], pin[1]); err != nil {
			t.Fatal(err)
		}
	}
	// A blob this node TOOK from the remote peer and now holds: never offered
	// back, or the two sites would keep each other's pins alive forever.
	taken := hashOf('d')
	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, all,
		[]placementsync.Entry{{BlobHash: taken, Size: 1024, Held: true}}); err != nil {
		t.Fatal(err)
	}
	h.holdOn(t, taken, self)

	got, err := h.cat.OfferPlacements(ctx, remotePeer, false, all)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{devicePinned}; !equalStrings(hashesOf(got), want) {
		t.Fatalf("offered to a non-sibling = %v, want %v", hashesOf(got), want)
	}

	got, err = h.cat.OfferPlacements(ctx, remotePeer, true, all)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{devicePinned, uploaded, notHeld}; !equalStrings(hashesOf(got), want) {
		t.Fatalf("offered to a sibling = %v, want %v", hashesOf(got), want)
	}
	for _, e := range got {
		if want := e.BlobHash != notHeld; e.Held != want {
			t.Fatalf("offer of %s says held=%v, want %v", e.BlobHash, e.Held, want)
		}
	}

	got, err = h.cat.OfferPlacements(ctx, remotePeer, true, placementsync.Range{Lo: "b", Hi: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{uploaded}; !equalStrings(hashesOf(got), want) {
		t.Fatalf("offered in [b, c) = %v, want %v", hashesOf(got), want)
	}
}

// TestReplacePlacementsTakesAndWithdraws is the destination's half: taking an
// offer records the placement, a blobs row from the size hint and the source's
// present replica, so the planner can name the source; a later answer without
// it withdraws only the placement; and a range replace never touches another
// range.
func TestReplacePlacementsTakesAndWithdraws(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)

	a, f := hashOf('a'), hashOf('f')
	rA := placementsync.Range{Lo: "a", Hi: "b"}
	rF := placementsync.Range{Lo: "f"}
	added, removed, err := h.cat.ReplacePlacements(ctx, remotePeer, rA, []placementsync.Entry{{BlobHash: a, Size: 77, Held: true}})
	if err != nil || added != 1 || removed != 0 {
		t.Fatalf("take: added=%d removed=%d err=%v, want 1, 0", added, removed, err)
	}
	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, rF, []placementsync.Entry{{BlobHash: f, Size: 5, Held: true}}); err != nil {
		t.Fatal(err)
	}

	// The planner pulls a, here, from the remote peer.
	plan, err := h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gapPairs(plan.Gaps), []string{a + "@" + self, f + "@" + self}; !equalStrings(got, want) {
		t.Fatalf("gaps = %v, want %v", got, want)
	}
	// The source's own claim, which is what BlobSources ranks from.
	var state string
	if err := h.db.Reader().QueryRow(`SELECT state FROM replicas WHERE blob_hash = ? AND peer_id = ?`,
		a, remotePeer).Scan(&state); err != nil || state != "present" {
		t.Fatalf("the source's replica of a taken blob = %q, %v, want present", state, err)
	}
	if n := h.eventsOfType(t, events.TypeSyncPlacementsExchanged); n != 2 {
		t.Fatalf("sync.placements_exchanged = %d, want 2", n)
	}

	// The same answer again changes nothing and emits nothing.
	added, removed, err = h.cat.ReplacePlacements(ctx, remotePeer, rA, []placementsync.Entry{{BlobHash: a, Size: 77, Held: true}})
	if err != nil || added != 0 || removed != 0 {
		t.Fatalf("repeat: added=%d removed=%d err=%v, want 0, 0", added, removed, err)
	}
	if n := h.eventsOfType(t, events.TypeSyncPlacementsExchanged); n != 2 {
		t.Fatalf("a repeated answer emitted: sync.placements_exchanged = %d, want 2", n)
	}

	// Withdrawn from [a, b): only a goes; f in another range stays.
	_, removed, err = h.cat.ReplacePlacements(ctx, remotePeer, rA, nil)
	if err != nil || removed != 1 {
		t.Fatalf("withdraw: removed=%d err=%v, want 1", removed, err)
	}
	taken, err := h.cat.TakenPlacements(ctx, remotePeer, all)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 1 || taken[0].BlobHash != f || !taken[0].Held {
		t.Fatalf("taken after withdrawal = %+v, want only %s, held by the source", taken, f)
	}

	// An entry outside the range it claims to answer is refused.
	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, rA,
		[]placementsync.Entry{{BlobHash: f, Size: 5, Held: true}}); err == nil {
		t.Fatal("an entry outside the range was accepted")
	}
}

// TestATakenPlacementRetainsItsBytes is the sabotage target (invariant 8): a
// blob this node took from a sibling has no asset and no local pin, so GC must
// count the taken placement as a reference or it reclaims bytes the other site
// is relying on this node to keep. It is also a vault blob, so a restricted
// principal may read it here (ADR-0104).
func TestATakenPlacementRetainsItsBytes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	h.seedRemotePeer(t)
	blob := hashOf('e')
	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, all,
		[]placementsync.Entry{{BlobHash: blob, Size: 1024, Held: true}}); err != nil {
		t.Fatal(err)
	}
	if n := refCount(t, h, blob); n != 1 {
		t.Fatalf("references = %d, want 1 (the taken placement)", n)
	}
	b, err := h.cat.Blob(ctx, hashing.MustParse(blob))
	if err != nil || b.References != 1 {
		t.Fatalf("Blob().References = %d, %v, want 1", b.References, err)
	}
	pinned, err := h.cat.SelfPinnedHashes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pinned[blob]; !ok {
		t.Fatal("a taken placement is not spared by the orphan sweep")
	}
	vault, err := h.cat.IsVaultBlob(ctx, blob)
	if err != nil || !vault {
		t.Fatalf("IsVaultBlob = %v, %v, want true", vault, err)
	}

	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, all, nil); err != nil {
		t.Fatal(err)
	}
	if n := refCount(t, h, blob); n != 0 {
		t.Fatalf("references after withdrawal = %d, want 0 (GC's grace window takes it from here)", n)
	}
}

// TestPlacementStatusOf reads where a blob is wanted and held: a self-pinned
// blob is wanted at the configured siblings too; another peer's copy counts only
// while its claim is fresh; and sites are failure domains, not peers.
func TestPlacementStatusOf(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	blob := hashOf('7')
	h.seedBlobs(t, blob)
	h.holdOn(t, blob, self)
	if err := h.cat.PinPlacement(ctx, blob, self); err != nil {
		t.Fatal(err)
	}

	st, err := h.cat.PlacementStatusOf(ctx, blob, []string{remotePeer})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{self, remotePeer}; !equalStrings(st.PinnedTo, want) {
		t.Fatalf("pinned_to = %v, want %v", st.PinnedTo, want)
	}
	if want := []string{self}; !equalStrings(st.HeldAt, want) || st.Sites != 1 {
		t.Fatalf("held_at = %v (sites %d), want %v at 1 site", st.HeldAt, st.Sites, want)
	}

	// The remote copy is recorded but never confirmed: not counted.
	h.holdOn(t, blob, remotePeer)
	if st, _ = h.cat.PlacementStatusOf(ctx, blob, nil); st.Sites != 1 {
		t.Fatalf("an unconfirmed remote claim counted: sites = %d, want 1", st.Sites)
	}
	// Confirmed just now (the harness clock): two sites.
	h.exec(t, `UPDATE replicas SET reported_at = ? WHERE peer_id = ?`, stamp, remotePeer)
	if st, _ = h.cat.PlacementStatusOf(ctx, blob, nil); st.Sites != 2 || len(st.HeldAt) != 2 {
		t.Fatalf("held_at = %v, sites = %d, want both peers at 2 sites", st.HeldAt, st.Sites)
	}
	// A third copy at the remote peer's own site adds a peer, not a site.
	h.exec(t, `INSERT INTO peers (id, name, site, mode, is_self, created_at, enrolled_at)
		VALUES ('same-site-peer', 'twin', 'site-b', 'full', 0, ?, ?)`, stamp, stamp)
	h.exec(t, `INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, reported_at, updated_at)
		VALUES (?, 'same-site-peer', 'present', 1024, ?, ?)`, blob, stamp, stamp)
	if st, _ = h.cat.PlacementStatusOf(ctx, blob, nil); st.Sites != 2 || len(st.HeldAt) != 3 {
		t.Fatalf("held_at = %v, sites = %d, want 3 peers at 2 sites", st.HeldAt, st.Sites)
	}
}

// TestAnOfferReconfirmsAReplicaMarkedMissing: the source's replica was marked
// missing meanwhile (a failed read), and the source still offers the blob as
// held. Its answer moves the replica back to present, so this node can plan a
// pull from it again.
func TestAnOfferReconfirmsAReplicaMarkedMissing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	h.seedRemotePeer(t)
	blob := hashOf('4')
	r := placementsync.Range{Lo: "4", Hi: "5"}
	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, r,
		[]placementsync.Entry{{BlobHash: blob, Size: 8, Held: true}}); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `UPDATE replicas SET state = 'missing' WHERE peer_id = ?`, remotePeer)
	before := h.eventsOfType(t, events.TypeReplicaPresent)
	if err := h.cat.ConfirmOffered(ctx, remotePeer, r); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := h.db.Reader().QueryRow(`SELECT state FROM replicas WHERE peer_id = ?`, remotePeer).Scan(&state); err != nil || state != "present" {
		t.Fatalf("the source's replica = %q, %v, want present again", state, err)
	}
	if n := h.eventsOfType(t, events.TypeReplicaPresent); n != before+1 {
		t.Fatalf("replica.present events = %d, want %d: the transition must emit", n, before+1)
	}
}

// TestAVerifiedTransferCorrectsAnOfferedSize: an offer's size is a hint; the
// length of bytes that verified by digest replaces it, so a later piece
// transfer does not plan from a sibling's mistake.
func TestAVerifiedTransferCorrectsAnOfferedSize(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	blob := hashOf('3')
	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, all,
		[]placementsync.Entry{{BlobHash: blob, Size: 999, Held: true}}); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.RecordBlobTransferred(ctx, catalog.BlobTransfer{
		BlobHash: blob, SourcePeerID: remotePeer, DestinationPeerID: self, Bytes: 1024,
	}); err != nil {
		t.Fatal(err)
	}
	var size int64
	if err := h.db.Reader().QueryRow(`SELECT size FROM blobs WHERE hash = ?`, blob).Scan(&size); err != nil || size != 1024 {
		t.Fatalf("blobs.size = %d, %v, want the verified 1024", size, err)
	}
	if n := h.eventsOfType(t, events.TypeBlobSizeCorrected); n != 1 {
		t.Fatalf("blob.size_corrected = %d, want 1", n)
	}
}

// TestASourceThatLosesItsCopyKeepsThePlacement is the review's case: the source
// lost or quarantined its copy and still wants the blob kept. The placement
// stays (this node's copy may be the only good one), the source's replica moves
// to missing, and with no other holder the pull is not planned to fail forever.
func TestASourceThatLosesItsCopyKeepsThePlacement(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	blob := hashOf('5')
	r := placementsync.Range{Lo: "5", Hi: "6"}
	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, r,
		[]placementsync.Entry{{BlobHash: blob, Size: 8, Held: true}}); err != nil {
		t.Fatal(err)
	}
	added, removed, err := h.cat.ReplacePlacements(ctx, remotePeer, r,
		[]placementsync.Entry{{BlobHash: blob, Size: 8, Held: false}})
	if err != nil || added != 0 || removed != 0 {
		t.Fatalf("a hold change: added=%d removed=%d err=%v, want 0, 0", added, removed, err)
	}
	var state string
	if err := h.db.Reader().QueryRow(`SELECT state FROM replicas WHERE blob_hash = ? AND peer_id = ?`,
		blob, remotePeer).Scan(&state); err != nil || state != "missing" {
		t.Fatalf("the source's replica = %q, %v, want missing", state, err)
	}
	if n := refCount(t, h, blob); n != 1 {
		t.Fatalf("references = %d, want 1: the placement still retains it", n)
	}
	plan, err := h.cat.PlanPeerConvergence(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Gaps) != 0 {
		t.Fatalf("gaps = %v, want none while no peer holds it", gapPairs(plan.Gaps))
	}

	// This node holds it: it is now the source the sibling heals from, and the
	// sibling's own self-pin is a gap THERE, which its queue plans.
	h.holdOn(t, blob, self)
	claim, err := h.cat.HeldClaim(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(claim) != 1 || claim[0].BlobHash != blob {
		t.Fatalf("held claim = %+v, want the taken blob this node holds", claim)
	}
}

// TestUnlistingASiblingDropsWhatItOffered: a node keeps only what its configured
// siblings offered. A peer it stopped listing is released, and its bytes go by
// GC's grace window.
func TestUnlistingASiblingDropsWhatItOffered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	h.seedRemotePeer(t)
	blob := hashOf('6')
	if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, all,
		[]placementsync.Entry{{BlobHash: blob, Size: 8, Held: true}}); err != nil {
		t.Fatal(err)
	}
	n, err := h.cat.DropPlacementsFromOthers(ctx, []string{remotePeer})
	if err != nil || n != 0 {
		t.Fatalf("keeping the listed sibling dropped %d, %v, want 0", n, err)
	}
	n, err = h.cat.DropPlacementsFromOthers(ctx, nil)
	if err != nil || n != 1 {
		t.Fatalf("unlisting every sibling dropped %d, %v, want 1", n, err)
	}
	if refs := refCount(t, h, blob); refs != 0 {
		t.Fatalf("references = %d after unlisting, want 0", refs)
	}
}

// TestARestatedSizeIsKeptAsOffered: a source correcting an offered size without
// changing its hold is recorded, so the next exchange's digest matches the
// source's and the range is not resent in full every cycle.
func TestARestatedSizeIsKeptAsOffered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	h.seedRemotePeer(t)
	blob := hashOf('2')
	r := placementsync.Range{Lo: "2", Hi: "3"}
	for _, size := range []int64{999, 1024} {
		if _, _, err := h.cat.ReplacePlacements(ctx, remotePeer, r,
			[]placementsync.Entry{{BlobHash: blob, Size: size, Held: true}}); err != nil {
			t.Fatal(err)
		}
	}
	taken, err := h.cat.TakenPlacements(ctx, remotePeer, r)
	if err != nil {
		t.Fatal(err)
	}
	offered := []placementsync.Entry{{BlobHash: blob, Size: 1024, Held: true}}
	if placementsync.Digest(taken) != placementsync.Digest(offered) {
		t.Fatalf("taken = %+v, want it restated as %+v", taken, offered)
	}
}
