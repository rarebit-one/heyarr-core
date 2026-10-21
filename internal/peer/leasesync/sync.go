package leasesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// Cache is where fetched leases land. *leases.CacheStore satisfies it: Refresh
// keeps only what verifies against the source's pinned key and records a
// lease.cached event when the cache changed.
type Cache interface {
	Refresh(ctx context.Context, sourcePeer string, tokens []string, now time.Time) (leases.CacheChange, error)
}

// Siblings lists the peers to fetch from, read fresh each pass so a peer
// removed from membership simply stops being fetched.
type Siblings interface {
	Siblings(ctx context.Context) ([]Target, error)
}

// Clock is injected so a test decides what "now" a fetched lease is verified
// at (ADR-0017).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Result counts one pass.
type Result struct {
	// Fetched is the siblings that answered and whose answer was applied.
	Fetched int
	// Deferred is the siblings that did not answer this pass. Their cached
	// leases are kept as they were.
	Deferred int
	// Changed is the siblings whose cache the pass changed.
	Changed int
	// Rejected is the served tokens, across all siblings, that did not verify
	// and were not cached.
	Rejected int
}

// Syncer mirrors each sibling's served device write leases into this peer's
// cache. It is leaderless and idempotent: re-fetching an unchanged set writes
// nothing. An unreachable sibling is an ordinary fact (ADR-0038): it is logged,
// its cached leases are kept (they expire on their own), and the pass moves on.
// A failed fetch never clears a cache.
type Syncer struct {
	cache    Cache
	fetch    Fetcher
	siblings Siblings
	clock    Clock
	log      *slog.Logger
}

// NewSyncer builds a Syncer. A nil clock is the wall clock; a nil logger
// discards.
func NewSyncer(cache Cache, fetch Fetcher, siblings Siblings, clock Clock, log *slog.Logger) *Syncer {
	if clock == nil {
		clock = systemClock{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Syncer{cache: cache, fetch: fetch, siblings: siblings, clock: clock, log: log}
}

// SyncAll runs one pass over every sibling. It errors only on a local failure
// (listing siblings, or writing the cache); a sibling that does not answer is
// counted as Deferred.
func (s *Syncer) SyncAll(ctx context.Context) (Result, error) {
	var res Result
	targets, err := s.siblings.Siblings(ctx)
	if err != nil {
		return res, fmt.Errorf("leasesync: listing siblings: %w", err)
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		source := identity.FormatPublicKey(t.Peer.PublicKey)
		tokens, err := s.fetch.FetchLeases(ctx, t)
		if err != nil {
			s.log.Info("lease fetch deferred a sibling; its cached leases are kept",
				"peer_id", t.Peer.PeerID, "error", err)
			res.Deferred++
			continue
		}
		change, err := s.cache.Refresh(ctx, source, tokens, s.clock.Now())
		if errors.Is(err, leases.ErrUnpinnedSource) {
			// Removed from membership between the listing and now: nothing it
			// served can verify, and its cache is not this pass's to clear.
			s.log.Info("lease fetch skipped a sibling that is no longer pinned", "peer_id", t.Peer.PeerID)
			res.Deferred++
			continue
		}
		if err != nil {
			return res, fmt.Errorf("leasesync: caching leases from %s: %w", t.Peer.PeerID, err)
		}
		res.Fetched++
		res.Rejected += change.Rejected
		if change.Changed() {
			res.Changed++
		}
		if change.Rejected > 0 {
			s.log.Warn("a sibling served leases that did not verify; they were not cached",
				"peer_id", t.Peer.PeerID, "rejected", change.Rejected)
		}
	}
	return res, nil
}
