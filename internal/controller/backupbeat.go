package controller

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/backupsync"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// beatClock is the wall clock the distribution cycle stamps beliefs with.
type beatClock struct{}

func (beatClock) Now() time.Time { return time.Now().UTC() }

// startBackup runs the continuous control-plane backup (§49, ADR-0044) — the
// beat that turns the configured interval into an RPO bound.
//
// # Why the controller, and not a worker
//
// A backup is a VACUUM INTO of the controller's OWN database. Unlike
// reconciliation or provider health, there is no other role to hand it to: a
// worker is a different process with no writer for this database (ADR-0002), so
// the work has to happen where the database is. That does not cross invariant 4
// — nothing is handed between roles — it is the controller maintaining its own
// store, the same category as the checkpoint sqlite.DB.Close performs.
//
// # No backup at startup, on purpose
//
// The provider-health beat enqueues at startup because a job insert is cheap.
// A backup is a whole-database VACUUM, and taking one on every process bounce
// would spend real work for a copy identical to the last whenever nothing
// changed (Take is idempotent per generation, so it would be deduplicated — but
// the VACUUM still runs to discover that). The cadence is the interval; an
// operator who wants one now runs `heyarr backup`.
//
// # Take, then distribute
//
// Each cycle takes a backup and then pushes it to every trusted Full Peer (§50,
// ADR-0046). A node with no peers pushes to nobody and the cycle is just the
// backup. The push never blocks the beat on an unreachable peer (ADR-0041): the
// distribution makes progress with whoever it has and records the rest.
func startBackup(ctx context.Context, db *sqlite.DB, eventLog *events.Log,
	dataDir, dir string, interval time.Duration, selfPeerID string, log *slog.Logger,
	material *mtls.Material, members *membership.Store,
) {
	if interval <= 0 {
		log.Info("continuous backup is disabled (backup.interval is empty or 0); " +
			"`heyarr backup` still takes one on demand")
		return
	}

	// The signer is optional for a LOCAL backup, but a backup that crosses to a
	// peer must be signed (peers reject an unsigned one), so distribution only
	// runs when the key is present — which it is whenever the peer surface is.
	var signer ed25519.PrivateKey
	if s, err := identity.Signer(dataDir); err == nil {
		signer = s
	}
	beliefs := backupsync.NewBeliefs(db)
	pusher := backupsync.NewPusher(material, log)

	cycle := func(ctx context.Context) error {
		art, err := backup.Take(ctx, backup.TakeOptions{
			DB:           db,
			Events:       eventLog,
			SourcePeerID: selfPeerID,
			Signer:       signer,
			Dir:          dir,
		})
		if err != nil {
			return err
		}
		if material == nil || members == nil || signer == nil {
			return nil // no peer surface, or nothing to sign a crossing backup with
		}
		all, err := members.List(ctx)
		if err != nil {
			// A membership read failing does not undo the backup that was taken.
			log.Warn("could not list peers to distribute the backup to", "error", err)
			return nil
		}
		targets := backupsync.FullPeerTargets(all)
		if len(targets) > 0 {
			backupsync.Distribute(ctx, pusher, beliefs, targets, art.Dir, beatClock{}, log)
		}
		return nil
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		backup.RunCadence(ctx, ticker.C, cycle, log)
	}()
	log.Info("continuous backup beat started", "interval", interval, "dir", dir)
}
