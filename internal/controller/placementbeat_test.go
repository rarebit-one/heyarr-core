package controller

import (
	"context"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
	"github.com/rarebit-one/heyarr-core/internal/peer/placementsync"
)

type fakePlacementCatalog struct {
	reports []inventory.Report
	askers  []string
	sibling []bool
	offered []placementsync.Entry
	present []string
}

func (f *fakePlacementCatalog) ReconcileInventory(_ context.Context, peerID string, r inventory.Report) (inventory.Outcome, error) {
	f.askers = append(f.askers, peerID)
	f.reports = append(f.reports, r)
	return inventory.Outcome{PeerID: peerID}, nil
}

func (f *fakePlacementCatalog) PresentReplicasIn(context.Context, string, placementsync.Range) ([]string, error) {
	return f.present, nil
}

func (f *fakePlacementCatalog) OfferPlacements(_ context.Context, asker string, sibling bool, _ placementsync.Range) ([]placementsync.Entry, error) {
	f.sibling = append(f.sibling, sibling)
	return f.offered, nil
}

// TestPlacementOffersFoldTheClaimAndAnswerUnchanged: the asker's held claim is
// folded as an incremental inventory under the asker (ADR-0114); a sibling is
// offered the node-policy set and a non-sibling is not; and a matching digest
// is answered "unchanged" with no entries.
func TestPlacementOffersFoldTheClaimAndAnswerUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blob := "blake3:" + "b" + "000000000000000000000000000000000000000000000000000000000000000"
	gone := "blake3:" + "b" + "111111111111111111111111111111111111111111111111111111111111111"
	cat := &fakePlacementCatalog{offered: []placementsync.Entry{{BlobHash: blob, Size: 9, Held: true}}, present: []string{blob, gone}}
	p := newPlacementOffers(cat, []string{"sibling-id"})
	r := placementsync.Range{Lo: "b", Hi: "c"}

	offer, err := p.OfferPlacements(ctx, "sibling-id", placementsync.Ask{
		Range: r, Digest: placementsync.Digest(nil), ObservedAt: time.Unix(1_700_000_000, 0), Claim: true,
		Held: []inventory.Entry{{BlobHash: blob, State: inventory.StatePresent, BytesPresent: 9}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if offer.Unchanged || len(offer.Entries) != 1 {
		t.Fatalf("first answer = %+v, want the one entry", offer)
	}
	if len(cat.reports) != 1 || cat.askers[0] != "sibling-id" || cat.reports[0].Mode != inventory.ModeIncremental {
		t.Fatalf("held claim folded as %+v by %v, want one incremental report for the asker", cat.reports, cat.askers)
	}
	// The claim is complete: a present replica it omits (gone) is now missing.
	if len(cat.reports[0].Entries) != 2 || cat.reports[0].Entries[1].BlobHash != gone ||
		cat.reports[0].Entries[1].State != inventory.StateMissing {
		t.Fatalf("folded entries = %+v, want the claim plus %s missing", cat.reports[0].Entries, gone)
	}
	if !cat.sibling[0] {
		t.Fatal("a configured sibling was not offered the node-policy set")
	}

	offer, err = p.OfferPlacements(ctx, "stranger-id", placementsync.Ask{Range: r, Digest: offer.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if !offer.Unchanged || len(offer.Entries) != 0 {
		t.Fatalf("a matching digest answered %+v, want unchanged with no entries", offer)
	}
	if cat.sibling[1] {
		t.Fatal("a peer not in placement.siblings was offered the node-policy set")
	}
	if err := offer.Validate(r); err != nil {
		t.Fatalf("the unchanged answer is not valid on the wire: %v", err)
	}
}
