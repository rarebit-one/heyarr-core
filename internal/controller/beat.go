package controller

import (
	"context"
	"log/slog"
	"time"
)

// tickerFunc makes a beat's clock: the channel it fires on at the given
// interval, and the stop that releases it. Production passes wallTicker; a
// test passes a channel it drives by hand, so a beat is observed taking a tick
// rather than slept out.
type tickerFunc func(interval time.Duration) (tick <-chan time.Time, stop func())

// wallTicker is the real clock every beat runs on outside a test.
func wallTicker(interval time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(interval)
	return ticker.C, ticker.Stop
}

// beat describes one of the controller's periodic loops. Every beat has the
// same shape — an optional pass at startup, then a pass per tick until the
// serving context ends — and differs only in what a pass does and what its
// started line says.
type beat struct {
	// name is what the started line calls it: "<name> beat started".
	name     string
	interval time.Duration
	// startup runs pass("startup") synchronously before the beat starts.
	// Whether a restart is a reason to run is each beat's own decision — see
	// startUpgradeScan and startBackup for the ones that deliberately wait.
	startup bool
	// attrs are logged on the started line after "interval".
	attrs []any
	// pass is one unit of the beat's work; reason is "startup" or "beat".
	pass func(reason string)
}

// startBeat runs b: its startup pass (if any) now, then one pass per tick of a
// ticker made by newTicker, in its own goroutine, until ctx is done — at which
// point the ticker is stopped. A pass is never fatal; the beat logs its own
// failures and the next tick tries again.
func startBeat(ctx context.Context, log *slog.Logger, newTicker tickerFunc, b beat) {
	if b.startup {
		b.pass("startup")
	}
	tick, stop := newTicker(b.interval)
	go func() {
		defer stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick:
				b.pass("beat")
			}
		}
	}()
	log.Info(b.name+" beat started", append([]any{"interval", b.interval}, b.attrs...)...)
}

// cadencePass adapts a peer-sync cycle — the backup, personal-state and
// catalog-ops beats — to a beat's pass. Its warning is the one those beats have
// always logged through backup.RunCadence, kept word for word.
func cadencePass(ctx context.Context, cycle func(context.Context) error, log *slog.Logger) func(string) {
	return func(string) {
		if err := cycle(ctx); err != nil {
			log.Warn("periodic control-plane backup failed; the next tick will try again", "error", err)
		}
	}
}
