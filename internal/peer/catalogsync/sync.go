package catalogsync

import (
	"context"
	"fmt"
	"log/slog"
)

// Store is the local editorial op-log this node converges. catalogtomb.Store
// satisfies it: Ops is what this node offers a sibling, RecordOps merges what the
// sibling answers.
type Store interface {
	Ops(ctx context.Context) ([]string, error)
	RecordOps(ctx context.Context, ops []string) error
}

// Siblings lists the peers this node converges its catalog with — every Full Peer
// but this one, read fresh each cycle so a peer removed from membership simply
// stops appearing.
type Siblings interface {
	Siblings(ctx context.Context) ([]Target, error)
}

// Syncer converges this node's catalog op-log with each sibling: it offers this
// node's ops and records the merged set the sibling answers with. Leaderless and
// idempotent — a converged pair is a no-op, and an unreachable sibling is a
// deferred fact, never an error that stops the cycle (ADR-0038). It errors only
// on a genuine LOCAL store failure, which the caller's cadence logs and retries.
type Syncer struct {
	store    Store
	exchange Exchanger
	siblings Siblings
	log      *slog.Logger
}

// NewSyncer builds a Syncer.
func NewSyncer(store Store, exchange Exchanger, siblings Siblings, log *slog.Logger) *Syncer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Syncer{store: store, exchange: exchange, siblings: siblings, log: log}
}

// SyncAll runs one convergence pass over every sibling. It returns how many
// siblings converged and how many were deferred (unreachable this cycle). A
// local read/record failure is returned as an error; a sibling failure is not.
func (s *Syncer) SyncAll(ctx context.Context) (synced, deferred int, err error) {
	targets, err := s.siblings.Siblings(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("catalogsync: listing siblings: %w", err)
	}
	if len(targets) == 0 {
		return 0, 0, nil
	}
	ours, err := s.store.Ops(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("catalogsync: reading local ops: %w", err)
	}

	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		theirs, err := s.exchange.Exchange(ctx, t, ours)
		if err != nil {
			// An unreachable or refusing sibling is a recorded fact, not a fatal
			// error: the pair is simply not converged this cycle, and the next
			// cycle tries again (ADR-0038).
			s.log.Debug("catalog-ops sync deferred a sibling", "peer_id", t.Peer.PeerID, "error", err)
			deferred++
			continue
		}
		// Record what the sibling holds. RecordOps is idempotent by op hash, so
		// re-learning our own ops back costs nothing, and it is what applies a
		// delete the sibling made to this node's catalog.
		if err := s.store.RecordOps(ctx, theirs); err != nil {
			return synced, deferred, fmt.Errorf("catalogsync: recording ops from %s: %w", t.Peer.PeerID, err)
		}
		synced++
	}
	return synced, deferred, nil
}
