package controller

// leasefetchbeat.go is the fetching half of ADR-0048's lease cache (#285): on
// a cadence of minutes, this peer fetches each sibling's device write leases
// (GET /peer/v1/leases, over the pinned mTLS link) and mirrors the ones that
// name this site into its lease cache, so that a device a sibling authorised
// to write can still be lifted here once that sibling is unreachable. Nothing
// reads the cache on the request path yet; that is later work on #285.
//
// It is a controller beat for the reason the issuing beat is (leasebeat.go):
// the cache is a controller-database table behind the single writer, and the
// pass is a convergence that writes nothing when nothing changed, so a re-run
// after a crash or restart is safe. An unreachable sibling is ordinary
// (ADR-0038): its cached leases are kept and age out on their own.

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/leasesync"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// leaseFetchInterval is how often this peer fetches its siblings' leases:
// ADR-0048's "minutes", the issuing beat's cadence. It bounds how long a lease
// a sibling revoked stays in this peer's cache while the two can reach each
// other. A fetch at any moment caches a lease with at least
// DeviceWriteRefreshThreshold left, so the cadence only has to be well inside
// that threshold for the cache never to run dry while a sibling is up.
const leaseFetchInterval = 5 * time.Minute

// startSiblingLeaseFetch starts the lease fetch beat over its own handle on the
// cache table. selfKey is this peer's public key: the cache keeps only leases
// whose resource is this site. clock is nil outside a test.
func startSiblingLeaseFetch(ctx context.Context, db *sqlite.DB, eventLog *events.Log,
	selfKey ed25519.PublicKey, members *membership.Store, fetcher leasesync.Fetcher,
	clock leasesync.Clock, log *slog.Logger, newTicker tickerFunc,
) error {
	if len(selfKey) != ed25519.PublicKeySize {
		return fmt.Errorf("controller: the lease fetch beat needs this peer's public key")
	}
	cache, err := leases.NewCache(leases.CacheOptions{
		Writer: db.Writer(), Reader: db.Reader(), Events: eventLog,
		Siblings: siblingKeys{store: members},
		SelfID:   identity.FormatPublicKey(selfKey), SelfKey: selfKey,
	})
	if err != nil {
		return fmt.Errorf("controller: opening the lease cache: %w", err)
	}
	syncer := leasesync.NewSyncer(cache, fetcher, leaseSiblings{members: members}, clock, log)
	runLeaseFetchBeat(ctx, syncer, leaseFetchInterval, log, newTicker)
	return nil
}

// leaseFetcher is what the beat drives. *leasesync.Syncer satisfies it.
type leaseFetcher interface {
	SyncAll(ctx context.Context) (leasesync.Result, error)
}

func runLeaseFetchBeat(ctx context.Context, f leaseFetcher, interval time.Duration, log *slog.Logger, newTicker tickerFunc) {
	startBeat(ctx, log, newTicker, beat{
		name: "sibling lease fetch", interval: interval,
		// At startup too, so a restarted node does not wait a whole interval
		// for a cache its siblings may already have changed; in the
		// background, because it dials them.
		startup: true, startupInBackground: true,
		pass: func(reason string) {
			res, err := f.SyncAll(ctx)
			if err != nil {
				log.Warn("sibling lease fetch failed; the next tick will try again", "reason", reason, "error", err)
				return
			}
			if res.Changed > 0 {
				log.Info("sibling lease cache updated", "reason", reason,
					"fetched", res.Fetched, "changed", res.Changed, "deferred", res.Deferred)
			}
		},
	})
}

// leaseSiblings lists the peers to fetch leases from: every member but this
// one that has a key and an endpoint, read fresh every pass. It is the set the
// issuing beat issues for (siblingKeys), so whatever a sibling issues for this
// site is fetched from it.
type leaseSiblings struct{ members *membership.Store }

func (l leaseSiblings) Siblings(ctx context.Context) ([]leasesync.Target, error) {
	ms, err := l.members.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("controller: listing peers for the lease fetch: %w", err)
	}
	var targets []leasesync.Target
	for _, m := range ms {
		if m.IsSelf || len(m.PublicKey) != ed25519.PublicKeySize || m.Endpoint == "" {
			continue
		}
		targets = append(targets, leasesync.Target{
			Peer:     mtls.Peer{PeerID: m.PeerID, Name: m.Name, PublicKey: m.PublicKey},
			Endpoint: m.Endpoint,
		})
	}
	return targets, nil
}
