package controller

// statereplbeat.go is the scheduled half of encrypted personal-state replication
// (§37, §45): personal state replicates to every trusted Full Peer BY DEFAULT, so
// this beat runs the same reconcile the POST /api/v1/state/replicate route runs on
// demand, on a cadence, without an operator triggering it. It mirrors startBackup
// (internal/controller/backupbeat.go) — a ticker driving a non-fatal cycle through
// the shared backup.RunCadence — and, like it, moves only opaque ciphertext.
//
// It is idempotent (a converged fleet is a no-op) and leaderless: an unreachable
// peer is a recorded fact, never an error that stops the cycle (ADR-0038), so the
// cycle only errors on a genuine local store failure, which RunCadence logs and
// retries. The manual route stays for demos and force-syncs.

import (
	"context"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/replication"
	psstore "github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// startStatePlaneReplication launches the encrypted-state replication beat. A
// non-positive interval disables it (the on-demand route still works); a node
// with no certificate material has no peer surface to replicate to, so it is a
// no-op there too.
func startStatePlaneReplication(ctx context.Context, db *sqlite.DB, eventLog *events.Log,
	interval time.Duration, selfPeerID string, log *slog.Logger,
	material *mtls.Material, members *membership.Store,
) {
	if interval <= 0 {
		log.Info("personal-state replication beat is disabled (interval is 0); " +
			"POST /api/v1/state/replicate still runs it on demand")
		return
	}
	if material == nil || members == nil {
		// No peer surface: nothing to replicate to. The on-demand route answers
		// 503 in the same state, so this is a supported single-peer node, not a
		// wiring error.
		return
	}
	// Its own single-writer store over the same controller database (ADR-0003
	// holds — one writer per database), exactly as the peer sync surface and the
	// on-demand reconciler open theirs.
	store, err := psstore.New(psstore.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		log.Error("personal-state replication beat: opening the store failed; the beat will not run", "error", err)
		return
	}
	reconciler := replication.NewReconciler(
		store,
		replication.NewClient(material, log),
		fullPeerLister{members: members, self: selfPeerID},
		eventLog, log)
	runStateReplicationBeat(ctx, reconciler, interval, log)
}

// stateReconciler is the one thing the beat drives — the on-demand reconciler's
// own entry point. An interface so a test can assert the beat CALLS it on a tick
// (the mechanism-with-a-caller property #362 exists to guarantee) without a peer
// fabric. *replication.Reconciler satisfies it.
type stateReconciler interface {
	ReconcileAll(ctx context.Context) (replicated, deferred int, err error)
}

// runStateReplicationBeat drives one reconcile per tick until ctx is done, over
// the shared non-fatal cadence runner.
func runStateReplicationBeat(ctx context.Context, reconciler stateReconciler, interval time.Duration, log *slog.Logger) {
	cycle := func(ctx context.Context) error {
		replicated, deferred, err := reconciler.ReconcileAll(ctx)
		if err != nil {
			return err
		}
		if replicated > 0 || deferred > 0 {
			log.Debug("personal-state replication cycle", "replicated", replicated, "deferred", deferred)
		}
		return nil
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		backup.RunCadence(ctx, ticker.C, cycle, log)
	}()
	log.Info("personal-state replication beat started", "interval", interval)
}
