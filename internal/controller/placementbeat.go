package controller

// placementbeat.go is two-site vault placement on the personal profile
// (ADR-0114, #662): the beat that asks each sibling which blobs it wants kept
// here, and the adapter that answers a sibling asking this node.
//
// Both halves are deliberately thin. What a node offers, what it took, and the
// writer that replaces it per range are the catalog's (peerplacements.go); the
// question and its cycle are internal/peer/placementsync's. This file only
// decides who the siblings are and puts the two on a cadence.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/vaultplacement"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/placementsync"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// placementCatalog is what the offer adapter needs of the catalog.
// *catalog.Catalog satisfies it.
type placementCatalog interface {
	ReconcileInventory(ctx context.Context, peerID string, report inventory.Report) (inventory.Outcome, error)
	OfferPlacements(ctx context.Context, asker string, sibling bool, r placementsync.Range) ([]placementsync.Entry, error)
	PresentReplicasIn(ctx context.Context, peer string, r placementsync.Range) ([]string, error)
}

// placementOffers answers a sibling's question (peerapi.PlacementSource).
//
// The asker's held claim is its report about its own disk, so it is folded as
// an incremental inventory under the certificate's peer, exactly the trust an
// inventory push gets: it is how this node learns a blob is now at two sites.
type placementOffers struct {
	cat      placementCatalog
	siblings map[string]bool
}

func newPlacementOffers(cat placementCatalog, siblings []string) placementOffers {
	set := make(map[string]bool, len(siblings))
	for _, s := range siblings {
		set[s] = true
	}
	return placementOffers{cat: cat, siblings: set}
}

func (p placementOffers) OfferPlacements(
	ctx context.Context, asker string, ask placementsync.Ask,
) (placementsync.Offer, error) {
	if ask.Claim {
		// A complete claim for the range: what it lists is present, and any of
		// the asker's present replicas here it does not list is now missing,
		// including blobs this node no longer offers it.
		claimed := make(map[string]struct{}, len(ask.Held))
		entries := append([]inventory.Entry(nil), ask.Held...)
		for _, e := range ask.Held {
			claimed[e.BlobHash] = struct{}{}
		}
		present, err := p.cat.PresentReplicasIn(ctx, asker, ask.Range)
		if err != nil {
			return placementsync.Offer{}, err
		}
		for _, h := range present {
			if _, ok := claimed[h]; !ok {
				entries = append(entries, inventory.Entry{BlobHash: h, State: inventory.StateMissing})
			}
		}
		if len(entries) > 0 {
			if _, err := p.cat.ReconcileInventory(ctx, asker, inventory.Report{
				PeerID: asker, Mode: inventory.ModeIncremental, ObservedAt: ask.ObservedAt, Entries: entries,
			}); err != nil {
				return placementsync.Offer{}, err
			}
		}
	}
	entries, err := p.cat.OfferPlacements(ctx, asker, p.siblings[asker], ask.Range)
	if err != nil {
		return placementsync.Offer{}, err
	}
	offer := placementsync.Offer{Range: ask.Range, Digest: placementsync.Digest(entries)}
	if offer.Digest == ask.Digest {
		offer.Unchanged = true
		offer.Entries = []placementsync.Entry{}
		return offer, nil
	}
	offer.Entries = entries
	return offer, nil
}

// siblingTargets resolves the configured siblings against membership each
// cycle: a sibling enrolled after start is asked from the next cycle, and one
// listed but not enrolled (or not a Full Peer) is skipped and said so.
func siblingTargets(members *membership.Store, siblings []string, log *slog.Logger) placementsync.Targets {
	want := make(map[string]bool, len(siblings))
	for _, s := range siblings {
		want[s] = true
	}
	return func(ctx context.Context) ([]placementsync.Target, error) {
		ms, err := members.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("controller: listing peers for sibling placement: %w", err)
		}
		found := make(map[string]bool, len(want))
		var out []placementsync.Target
		for _, m := range ms {
			if !want[m.PeerID] || m.IsSelf {
				continue
			}
			found[m.PeerID] = true
			if m.Mode != "full" {
				log.Warn("a placement sibling is not a Full Peer, so it is not asked",
					"peer_id", m.PeerID, "mode", m.Mode)
				continue
			}
			out = append(out, placementsync.Target{
				Peer:     mtls.Peer{PeerID: m.PeerID, Name: m.Name, PublicKey: m.PublicKey},
				Endpoint: m.Endpoint,
			})
		}
		for s := range want {
			if !found[s] {
				log.Warn("placement.siblings names a peer that is not enrolled here", "peer_id", s,
					"note", "the id is this node's id for the peer, as `peers list` shows it")
			}
		}
		return out, nil
	}
}

// startSiblingPlacement launches the asking beat. A non-positive interval or an
// empty sibling list disables it; a node with no certificate material has no
// sibling to dial.
func startSiblingPlacement(ctx context.Context, db *sqlite.DB, eventLog *events.Log,
	interval time.Duration, siblings []string, peerName, peerSite string, log *slog.Logger,
	material *mtls.Material, members *membership.Store,
) {
	cat, err := catalog.New(catalog.Options{DB: db, Events: eventLog, PeerName: peerName, PeerSite: peerSite, Logger: log})
	if err != nil {
		log.Error("sibling placement: opening the catalog failed; the beat will not run", "error", err)
		return
	}
	// A sibling the operator stopped listing no longer decides what is kept
	// here (ADR-0114). Run even when nothing is listed, which is how the last
	// sibling is unlisted.
	if n, err := cat.DropPlacementsFromOthers(ctx, siblings); err != nil {
		log.Error("sibling placement: dropping placements from unlisted peers failed", "error", err)
	} else if n > 0 {
		log.Info("dropped placements taken from peers no longer listed in placement.siblings",
			"placements", n, "note", "their bytes go by GC's grace window")
	}
	if len(siblings) == 0 {
		return
	}
	if interval <= 0 {
		log.Info("sibling placement asking is disabled (placement.interval is 0); siblings asking this node are still answered")
		return
	}
	if material == nil || members == nil {
		log.Warn("placement.siblings is set but this node has no peer surface, so no sibling can be asked")
		return
	}
	puller := placementsync.NewPuller(cat, placementsync.NewClient(material, log),
		siblingTargets(members, siblings, log), time.Now, log)
	runSiblingPlacementBeat(ctx, puller, interval, log, wallTicker)
}

// placementCycler is what the beat drives. *placementsync.Puller satisfies it.
type placementCycler interface {
	Cycle(ctx context.Context) (placementsync.Summary, error)
}

func runSiblingPlacementBeat(ctx context.Context, p placementCycler, interval time.Duration,
	log *slog.Logger, newTicker tickerFunc,
) {
	cycle := func(ctx context.Context) error {
		sum, err := p.Cycle(ctx)
		if err != nil {
			return err
		}
		if sum.Changed > 0 {
			log.Info("took placements from siblings",
				"added", sum.Added, "withdrawn", sum.Removed, "ranges_changed", sum.Changed)
		}
		return nil
	}
	startBeat(ctx, log, newTicker, beat{
		name: "sibling placement", interval: interval, pass: cadencePass(ctx, cycle, log),
	})
}

// placementStatus adapts the catalog to vaultplacement.StatusReader, adding
// this node's configured siblings: a blob pinned here is wanted there too.
type placementStatus struct {
	cat      *catalog.Catalog
	siblings []string
}

func (p placementStatus) PlacementStatus(ctx context.Context, blobHash string) (vaultplacement.Status, error) {
	st, err := p.cat.PlacementStatusOf(ctx, blobHash, p.siblings)
	if err != nil {
		return vaultplacement.Status{}, err
	}
	return vaultplacement.Status{PinnedTo: st.PinnedTo, HeldAt: st.HeldAt, TakenFrom: st.TakenFrom, Sites: st.Sites}, nil
}
