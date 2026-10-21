package controller

// leasebeat.go is the re-issuer ADR-0048 owes (#285): on a cadence of minutes,
// this peer converges its device write leases on its management grants
// (ADR-0065) and its siblings. A device authorised for write here gets, for
// every sibling site, a lease "device D may write at site B", re-issued before
// it is worth less than the refresh threshold and revoked when the grant is
// deleted. A sibling fetches the leases (GET /peer/v1/leases; its fetch beat
// is leasefetchbeat.go) and will honour one only if it has opted into this
// peer as an issuer, which is later work.
//
// # Why a controller beat, and not a job
//
// Issuing is a controller-database mutation signed with this node's identity
// key, the same category as the backup and retention beats: only the
// controller holds the writer and the key, so a worker could not run it
// without crossing invariant 4. The pass is a convergence, idempotent by
// construction (a converged state writes nothing), so a pass re-run after a
// crash or a restart is safe, which is what a durable job would have bought.

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// deviceLeaseInterval is how often the device write lease beat runs: ADR-0048's
// "minutes". It bounds how long a deleted grant's leases stay served to a
// reachable sibling (one interval, plus that sibling's own fetch cadence). A
// refresh is due only every DeviceWriteTTL − DeviceWriteRefreshThreshold, so
// most passes are a read that writes nothing.
const deviceLeaseInterval = 5 * time.Minute

// startDeviceLeaseIssuer starts the device write lease beat over its own lease
// store, a second handle over the access_leases table the peer surface serves
// from (the pattern the guest minter already follows). clock is nil outside a
// test. It runs at startup too: a node that was down past a refresh catches up
// before its siblings next fetch.
func startDeviceLeaseIssuer(ctx context.Context, db *sqlite.DB, eventLog *events.Log,
	signer ed25519.PrivateKey, grants *catalog.Catalog, members *membership.Store,
	clock leases.Clock, log *slog.Logger, newTicker tickerFunc,
) error {
	store, err := leases.New(leases.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: eventLog,
		Signer: signer, Siblings: siblingKeys{store: members}, Clock: clock,
	})
	if err != nil {
		return fmt.Errorf("controller: opening the lease store for device write leases: %w", err)
	}
	issuer, err := leases.NewDeviceWriteIssuer(store, writeDeviceKeys{cat: grants}, siblingKeys{store: members})
	if err != nil {
		return fmt.Errorf("controller: building the device write lease issuer: %w", err)
	}
	runDeviceLeaseBeat(ctx, issuer, deviceLeaseInterval, log, newTicker)
	return nil
}

// deviceLeaseReconciler is what the beat drives. *leases.DeviceWriteIssuer
// satisfies it.
type deviceLeaseReconciler interface {
	Reconcile(ctx context.Context) (leases.DeviceWriteResult, error)
}

func runDeviceLeaseBeat(ctx context.Context, r deviceLeaseReconciler, interval time.Duration, log *slog.Logger, newTicker tickerFunc) {
	startBeat(ctx, log, newTicker, beat{
		name: "device write lease", interval: interval, startup: true,
		attrs: []any{"ttl", leases.DeviceWriteTTL, "refresh_below", leases.DeviceWriteRefreshThreshold},
		pass: func(reason string) {
			res, err := r.Reconcile(ctx)
			if err != nil {
				log.Warn("device write lease pass failed; the next tick will try again", "reason", reason, "error", err)
				return
			}
			if res.Issued > 0 || res.Revoked > 0 {
				log.Info("device write leases converged", "reason", reason,
					"issued", res.Issued, "reused", res.Reused, "revoked", res.Revoked)
			}
		},
	})
}

// writeDeviceKeys adapts the catalog's management grants (ADR-0065) to
// leases.WriteDevices: the device keys this node authorised for write, in the
// canonical form the grant stores.
type writeDeviceKeys struct{ cat *catalog.Catalog }

func (w writeDeviceKeys) WriteDeviceKeys(ctx context.Context) ([]string, error) {
	grants, err := w.cat.ListManagementGrants(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(grants))
	for i, g := range grants {
		keys[i] = g.DeviceKey
	}
	return keys, nil
}
