package backup

import (
	"context"
	"log/slog"
	"time"
)

// RunCadence takes a backup on every tick until ctx is cancelled. It is the
// "continuous" in §49's continuous backup — the loop that makes the RPO a
// bound rather than a hope.
//
// The ticks channel is a parameter rather than an internal time.NewTicker so
// the loop is testable without a clock that actually advances (ADR-0017): the
// controller passes a real ticker's channel, a test passes one it drives by
// hand. This is the same seam the event log and the job queue use, applied to
// the one beat that could not be a job (a backup operates on the controller's
// own database, so there is no other role to hand it to — invariant 4 is not
// crossed because nothing is crossed).
//
// A failed backup is logged and the loop continues, never fatal — the same
// stance the reconciliation and provider-health beats take (and ADR-0041's
// progress rule): a node that cannot take a backup right now still serves,
// still plays and still ingests, and the next tick tries again. What it must
// not do is take the controller down because a disk was briefly full.
func RunCadence(ctx context.Context, ticks <-chan time.Time, take func(context.Context) error, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if err := take(ctx); err != nil && log != nil {
				log.Warn("periodic control-plane backup failed; the next tick will try again", "error", err)
			}
		}
	}
}
