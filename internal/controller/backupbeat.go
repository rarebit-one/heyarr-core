package controller

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

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
func startBackup(ctx context.Context, db *sqlite.DB, eventLog *events.Log,
	dataDir, dir string, interval time.Duration, selfPeerID string, log *slog.Logger,
) {
	if interval <= 0 {
		log.Info("continuous backup is disabled (backup.interval is empty or 0); " +
			"`heyarr backup` still takes one on demand")
		return
	}

	// The signer is optional, exactly as it is for the CLI verb: a node without
	// its identity key on disk still backs up, just unsigned. A backup that must
	// cross to a peer is signed at that point (M7-03).
	var signer ed25519.PrivateKey
	if s, err := identity.Signer(dataDir); err == nil {
		signer = s
	}

	take := func(ctx context.Context) error {
		_, err := backup.Take(ctx, backup.TakeOptions{
			DB:           db,
			Events:       eventLog,
			SourcePeerID: selfPeerID,
			Signer:       signer,
			Dir:          dir,
		})
		return err
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		backup.RunCadence(ctx, ticker.C, take, log)
	}()
	log.Info("continuous backup beat started", "interval", interval, "dir", dir)
}
