package controller

// fanoutpeek.go is both ends of the origin asking its siblings what they did
// with an acquire op it published (ADR-0119 #924 amendment): the sibling's
// answer on its peer surface, and the origin's reader behind acquisition
// status. A read for display at both ends; nothing is written and nothing
// decides anything from it.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/siblingfanout"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// fanOutAnswers adapts the catalog to peerapi.FanOutSource: this node's
// outcomes for the acquire ops one sibling signed, with the state of the want
// each reached.
type fanOutAnswers struct {
	cat  *catalog.Catalog
	site string
}

func (f fanOutAnswers) FanOutFrom(ctx context.Context, signer, opHash string, limit int) (peerapi.FanOutAnswer, error) {
	entries, err := f.cat.ListFanOut(ctx, catalog.FanOutQuery{Signer: signer, OpHash: opHash, Limit: limit})
	if err != nil {
		return peerapi.FanOutAnswer{}, err
	}
	out := peerapi.FanOutAnswer{Site: f.site, Applications: make([]peerapi.FanOutApplication, 0, len(entries))}
	for _, e := range entries {
		a := peerapi.FanOutApplication{
			OpHash: e.OpHash, Outcome: string(e.Outcome), Detail: e.Detail,
			AppliedAt: e.AppliedAt.UTC().Format(time.RFC3339), ReportedUnsourced: e.Reported,
		}
		if e.DesiredItemID != "" {
			rec, err := f.cat.Acquisition(ctx, e.DesiredItemID)
			switch {
			case errors.Is(err, catalog.ErrNoAcquisition):
			case err != nil:
				return peerapi.FanOutAnswer{}, fmt.Errorf("controller: reading the want behind %s: %w", e.OpHash, err)
			default:
				a.State = rec.State.Name()
			}
		}
		out.Applications = append(out.Applications, a)
	}
	return out, nil
}

// fanOutTargets lists the siblings an origin asks: the peers it converges its
// catalog op-log with, which are the ones its acquire ops reach.
func fanOutTargets(members *membership.Store) siblingfanout.Targets {
	return func(ctx context.Context) ([]siblingfanout.Target, error) {
		ms, err := members.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("controller: listing peers for fan-out outcomes: %w", err)
		}
		var out []siblingfanout.Target
		for _, m := range ms {
			if m.IsSelf || !syncsCatalogOps(m.Mode) {
				continue
			}
			out = append(out, siblingfanout.Target{
				Peer:     mtls.Peer{PeerID: m.PeerID, Name: m.Name, PublicKey: m.PublicKey},
				Endpoint: m.Endpoint,
			})
		}
		return out, nil
	}
}

// siblingFanOut adapts siblingfanout.Reader to resources.SiblingFanOut.
type siblingFanOut struct{ r *siblingfanout.Reader }

func newSiblingFanOut(material *mtls.Material, members *membership.Store, log *slog.Logger) siblingFanOut {
	return siblingFanOut{r: siblingfanout.NewReader(siblingfanout.Options{
		Fetch:   siblingfanout.NewClient(material, log),
		Targets: fanOutTargets(members),
		Logger:  log,
	})}
}

func (s siblingFanOut) Outcomes(ctx context.Context, opHash string) ([]resources.SiblingOutcome, error) {
	got, err := s.r.Outcomes(ctx, opHash)
	if err != nil {
		return nil, err
	}
	out := make([]resources.SiblingOutcome, 0, len(got))
	for _, o := range got {
		out = append(out, resources.SiblingOutcome{
			PeerID: o.PeerID, Name: o.Name, Site: o.Site, Reachable: o.Reachable,
			Outcome: o.Outcome, Detail: o.Detail, AppliedAt: o.AppliedAt, State: o.State,
			ReportedUnsourced: o.ReportedUnsourced, AskedAt: o.AskedAt,
		})
	}
	return out, nil
}
