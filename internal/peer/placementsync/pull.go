package placementsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// Target is a sibling to ask: its pinned identity and where to dial it, kept
// together so a caller cannot pin one peer and dial another (ADR-0012).
type Target struct {
	Peer     mtls.Peer
	Endpoint string
}

// Asker puts one question to one sibling. An interface so the cycle can be
// tested without standing up mTLS; [Client] is the real implementation.
type Asker interface {
	Ask(ctx context.Context, t Target, a Ask) (Offer, error)
}

// LocalStore is this node's half of the exchange: what it took from each source,
// the single writer that replaces it, and its own claim. *catalog.Catalog
// satisfies it.
type LocalStore interface {
	// TakenPlacements is what this node took from source inside the range, in
	// hash order, as the source last offered it (Held is the SOURCE's).
	TakenPlacements(ctx context.Context, source string, r Range) ([]Entry, error)
	// HeldClaim is every blob inside the range this node holds present: its
	// complete claim about its own disk, sent to each source.
	HeldClaim(ctx context.Context, r Range) ([]Entry, error)
	// ReplacePlacements makes the placements taken from source inside the
	// range exactly entries, in one transaction, and says how many it added
	// and removed.
	ReplacePlacements(ctx context.Context, source string, r Range, entries []Entry) (added, removed int, err error)
	// ConfirmOffered records that the source just restated it holds every
	// blob this node took from it inside the range: the freshness GC's
	// last-copy rule reads (ADR-0018).
	ConfirmOffered(ctx context.Context, source string, r Range) error
}

// Targets lists the siblings to ask this cycle.
type Targets func(ctx context.Context) ([]Target, error)

// Summary is what one cycle did, for the beat's log.
type Summary struct {
	// Asked is how many sibling ranges were asked.
	Asked int
	// Changed is how many answers changed what this node took.
	Changed int
	// Added and Removed count placements across the cycle.
	Added, Removed int
	// Unreachable is how many siblings could not be asked this cycle. Not an
	// error (ADR-0038): a sibling not heard from is a recorded fact.
	Unreachable int
}

// Puller runs the destination's half: one cycle asks every sibling about every
// range and replaces what it took. Safe to run repeatedly; a converged pair
// costs one digest round trip per range.
type Puller struct {
	store   LocalStore
	asker   Asker
	targets Targets
	now     func() time.Time
	log     *slog.Logger

	// sentHeld is the held claim last acknowledged, per sibling and range, so
	// an unchanged claim is not resent every cycle. In memory: a restart
	// resends each claim once, which the source folds idempotently.
	mu       sync.Mutex
	sentHeld map[string]sentClaim
}

// sentClaim is one acknowledged claim: its digest, and when.
type sentClaim struct {
	digest string
	at     time.Time
}

// claimRefresh is how often an unchanged claim is resent anyway. The source
// counts this node's copies as durable only while its claim is fresh (the hour
// GC's last-copy rule uses, ADR-0018), so the claim is restated well inside it.
const claimRefresh = 30 * time.Minute

// NewPuller builds a puller. now is injected (ADR-0017).
func NewPuller(store LocalStore, asker Asker, targets Targets, now func() time.Time, log *slog.Logger) *Puller {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = time.Now
	}
	return &Puller{store: store, asker: asker, targets: targets, now: now, log: log, sentHeld: map[string]sentClaim{}}
}

// errLocal marks a failure of this node's own store, which is the one thing
// that stops a cycle: a sibling's failure is that sibling's.
var errLocal = errors.New("placementsync: local store")

// Cycle asks every sibling about every range once.
//
// It errors only on a local store failure. A sibling that cannot be reached, or
// that answers nonsense, is logged and counted, and the remaining siblings are
// still asked.
func (p *Puller) Cycle(ctx context.Context) (Summary, error) {
	var sum Summary
	targets, err := p.targets(ctx)
	if err != nil {
		return sum, err
	}
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return sum, nil
		}
		if err := p.askSibling(ctx, t, &sum); err != nil {
			if errors.Is(err, errLocal) {
				return sum, err
			}
			sum.Unreachable++
			p.log.Info("could not ask a sibling which placements it offers; it is asked again next cycle",
				"peer_id", t.Peer.PeerID, "peer_name", t.Peer.Name, "error", err)
		}
	}
	return sum, nil
}

func (p *Puller) askSibling(ctx context.Context, t Target, sum *Summary) error {
	for _, r := range Ranges() {
		if err := ctx.Err(); err != nil {
			return nil
		}
		taken, err := p.store.TakenPlacements(ctx, t.Peer.PeerID, r)
		if err != nil {
			return errors.Join(errLocal, err)
		}
		mine, err := p.store.HeldClaim(ctx, r)
		if err != nil {
			return errors.Join(errLocal, err)
		}
		held := make([]inventory.Entry, len(mine))
		for i, e := range mine {
			held[i] = inventory.Entry{BlobHash: e.BlobHash, State: inventory.StatePresent, BytesPresent: e.Size}
		}
		now := p.now().UTC()
		ask := Ask{Range: r, Digest: Digest(taken), ObservedAt: now}
		heldKey := t.Peer.PeerID + "\x00" + r.Lo
		heldDigest := Digest(mine)
		p.mu.Lock()
		last := p.sentHeld[heldKey]
		resend := last.digest != heldDigest || now.Sub(last.at) >= claimRefresh
		p.mu.Unlock()
		if resend {
			ask.Claim, ask.Held = true, held
		}
		if err := ask.Validate(); err != nil {
			return errors.Join(errLocal, err)
		}

		offer, err := p.asker.Ask(ctx, t, ask)
		if err != nil {
			return err
		}
		if err := offer.Validate(r); err != nil {
			return err
		}
		if offer.Unchanged && offer.Digest != ask.Digest {
			// "Unchanged" is a claim that the source's set is the one this node
			// already took. A digest that says otherwise is the source being
			// wrong, and keeping a stale set on its word would be worse.
			return fmt.Errorf("%w: %s answered unchanged with digest %s, this node took %s",
				ErrInvalid, t.Peer.PeerID, offer.Digest, ask.Digest)
		}
		sum.Asked++
		if resend {
			p.mu.Lock()
			p.sentHeld[heldKey] = sentClaim{digest: heldDigest, at: now}
			p.mu.Unlock()
		}
		if !offer.Unchanged {
			added, removed, err := p.store.ReplacePlacements(ctx, t.Peer.PeerID, r, offer.Entries)
			if err != nil {
				return errors.Join(errLocal, err)
			}
			if added > 0 || removed > 0 {
				sum.Changed++
				sum.Added += added
				sum.Removed += removed
			}
		}
		if err := p.store.ConfirmOffered(ctx, t.Peer.PeerID, r); err != nil {
			return errors.Join(errLocal, err)
		}
	}
	return nil
}
