package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// pinExpiryInterval is how often unconfirmed upload pins past their expiry are
// removed. Expiry is a day after upload, so a quarter of an hour late is
// nothing; what matters is that it happens at all.
const pinExpiryInterval = 15 * time.Minute

// startPinExpiry runs the beat that removes provisional upload pins their
// writer never confirmed (ADR-0096 amendment, #714), releasing their blobs to
// GC exactly as an unpin does. GC's own grace window still runs before any
// byte is unlinked (invariant 8).
//
// It runs only while placement.provisional_upload_pins is on. Off, nothing
// is expired, including pins that became provisional while it was on: the
// switch is the rollout's safety catch, and turning it off must stop the
// reclaiming as well as the recording.
//
// The controller does it, not a worker, for the backup beat's reason: it is
// the controller maintaining its own database, and only the controller holds
// the writer.
func startPinExpiry(ctx context.Context, db *sqlite.DB, eventLog *events.Log, enabled bool,
	peerName, peerSite string, log *slog.Logger, newTicker tickerFunc,
) {
	if !enabled {
		return
	}
	cat, err := catalog.New(catalog.Options{DB: db, Events: eventLog, PeerName: peerName, PeerSite: peerSite, Logger: log})
	if err != nil {
		log.Error("pin expiry: opening the catalog failed; the beat will not run", "error", err)
		return
	}
	startBeat(ctx, log, newTicker, beat{
		name: "upload pin expiry", interval: pinExpiryInterval,
		// A node that was down past some pins' expiry catches up at once.
		startup: true, startupInBackground: true,
		pass: func(string) {
			n, err := cat.ExpireProvisionalPins(ctx, time.Now())
			if err != nil {
				log.Warn("expiring unconfirmed upload pins failed; the next tick will try again", "error", err)
				return
			}
			if n > 0 {
				log.Info("released the blobs of unconfirmed vault uploads", "pins", n)
			}
		},
	})
}
