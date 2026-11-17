package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
)

// startPeerPullBeat enqueues a peer-pull pass (ADR-0119 amendment, #833) on the
// acquisition interval, and once at startup: a library opted in, or a hold a
// sibling published while this node was down, acts now rather than an
// interval from now. One enqueue, one dedupe key, so a slow pass cannot pile up
// behind itself. Only started when acquisition.peer_pull names a library; the
// worker registers the handler under the same condition.
func startPeerPullBeat(ctx context.Context, queue *jobs.Queue, interval time.Duration, log *slog.Logger, newTicker tickerFunc) {
	if interval <= 0 {
		log.Info("peer-pull beat is disabled (acquisition.interval is 0); " +
			"holds are neither published nor applied until it is enabled")
		return
	}
	startBeat(ctx, log, newTicker, beat{
		name: "peer-pull", interval: interval, startup: true,
		pass: func(reason string) {
			if err := enqueuePeerPull(ctx, queue); err != nil {
				log.Warn("could not enqueue a peer-pull pass", "reason", reason, "error", err)
			}
		},
	})
}

func enqueuePeerPull(ctx context.Context, queue *jobs.Queue) error {
	_, err := queue.Enqueue(ctx, jobs.EnqueueOptions{
		Type:      replication.PeerPullJobType,
		Payload:   struct{}{},
		DedupeKey: replication.PeerPullDedupeKey,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("controller: enqueueing a peer-pull pass: %w", err)
	}
	return nil
}
