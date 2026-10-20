package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
)

// startFanOutBeat enqueues an apply pass for siblings' acquire ops (ADR-0119)
// on the configured interval, and once at startup: a node that was down while
// a sibling grabbed a dozen releases should act on them now, not an interval
// from now. It is the reconciliation shape — one enqueue, one dedupe key — so
// a slow pass cannot pile up behind itself. Only started when
// acquisition.fan_out is on; the worker registers the handler under the same
// switch.
func startFanOutBeat(ctx context.Context, queue *jobs.Queue, interval time.Duration, log *slog.Logger, newTicker tickerFunc) {
	if interval <= 0 {
		log.Info("fan-out apply beat is disabled (acquisition.interval is 0); " +
			"siblings' acquire ops are stored but not applied until it is enabled")
		return
	}
	startBeat(ctx, log, newTicker, beat{
		name: "fan-out apply", interval: interval, startup: true,
		pass: func(reason string) {
			if err := enqueueApplyAcquire(ctx, queue); err != nil {
				log.Warn("could not enqueue a fan-out apply pass", "reason", reason, "error", err)
			}
		},
	})
}

func enqueueApplyAcquire(ctx context.Context, queue *jobs.Queue) error {
	_, err := queue.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      acquisition.ApplyAcquireJobType,
		Payload:   struct{}{},
		DedupeKey: acquisition.ApplyAcquireDedupeKey,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("controller: enqueueing a fan-out apply pass: %w", err)
	}
	return nil
}
