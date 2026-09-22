package controller

// catalogsyncbeat.go is the scheduled half of two-site catalog convergence
// (ADR-0073, #449): the editorial op-log converges between the two controllers of
// an active-active pair BY DEFAULT, so this beat runs the same exchange the
// POST /peer/v1/catalog/ops route runs on demand, on a cadence, without an
// operator triggering it. It mirrors startStatePlaneReplication — a ticker
// driving a non-fatal cycle through the shared backup.RunCadence.
//
// It is idempotent (a converged pair is a no-op) and leaderless: an unreachable
// sibling is a recorded fact, never an error that stops the cycle (ADR-0038), so
// the cycle only errors on a genuine local store failure, which RunCadence logs
// and retries. The on-demand route stays for demos and force-syncs.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogtomb"
	"github.com/rarebit-one/heyarr-core/internal/peer/catalogsync"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// startCatalogOpsSync launches the catalog-ops convergence beat. A non-positive
// interval disables it (the on-demand route still works); a node with no
// certificate material has no peer surface and no sibling to converge with, so it
// is a no-op there too.
func startCatalogOpsSync(ctx context.Context, db *sqlite.DB, interval time.Duration,
	selfPeerID string, log *slog.Logger, material *mtls.Material, members *membership.Store,
) {
	if interval <= 0 {
		log.Info("catalog-ops sync beat is disabled (interval is 0); " +
			"POST /peer/v1/catalog/ops still converges the pair on demand")
		return
	}
	if material == nil || members == nil {
		// No peer surface: nobody to converge with. The on-demand route answers
		// 503 in the same state, so this is a supported single-site node, not a
		// wiring error.
		return
	}
	// Its own stateless handle over the same controller database, exactly as the
	// peer sync surface and the resources API open theirs.
	store, err := catalogtomb.New(catalogtomb.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		log.Error("catalog-ops sync beat: opening the store failed; the beat will not run", "error", err)
		return
	}
	syncer := catalogsync.NewSyncer(
		store,
		catalogsync.NewClient(material, log),
		catalogSiblings{members: members, self: selfPeerID},
		log)
	runCatalogOpsSyncBeat(ctx, syncer, interval, log)
}

// catalogSyncer is the one thing the beat drives — a seam a test asserts the beat
// CALLS on a tick (the mechanism-with-a-caller property #362) without a peer
// fabric. *catalogsync.Syncer satisfies it.
type catalogSyncer interface {
	SyncAll(ctx context.Context) (synced, deferred int, err error)
}

// runCatalogOpsSyncBeat drives one convergence pass per tick until ctx is done,
// over the shared non-fatal cadence runner.
func runCatalogOpsSyncBeat(ctx context.Context, syncer catalogSyncer, interval time.Duration, log *slog.Logger) {
	cycle := func(ctx context.Context) error {
		synced, deferred, err := syncer.SyncAll(ctx)
		if err != nil {
			return err
		}
		if synced > 0 || deferred > 0 {
			log.Debug("catalog-ops sync cycle", "synced", synced, "deferred", deferred)
		}
		return nil
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		backup.RunCadence(ctx, ticker.C, cycle, log)
	}()
	log.Info("catalog-ops sync beat started", "interval", interval)
}

// catalogSiblings enumerates the Full Peers this node converges its catalog with
// — every Full Peer but this one, read fresh every cycle so a peer removed from
// membership simply stops appearing, which is the whole of ADR-0012's revocation.
// It mirrors fullPeerLister, in catalogsync's own Target type.
type catalogSiblings struct {
	members *membership.Store
	self    string
}

func (l catalogSiblings) Siblings(ctx context.Context) ([]catalogsync.Target, error) {
	ms, err := l.members.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("controller: listing peers for catalog-ops sync: %w", err)
	}
	var targets []catalogsync.Target
	for _, m := range ms {
		if m.IsSelf || m.Mode != "full" {
			continue
		}
		targets = append(targets, catalogsync.Target{
			Peer:     mtls.Peer{PeerID: m.PeerID, Name: m.Name, PublicKey: m.PublicKey},
			Endpoint: m.Endpoint,
		})
	}
	return targets, nil
}
