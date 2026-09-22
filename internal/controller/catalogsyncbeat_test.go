package controller

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// countingCatalogSyncer records how many times the beat drove it.
type countingCatalogSyncer struct {
	calls  atomic.Int64
	fired  chan struct{}
	closed atomic.Bool
}

func (c *countingCatalogSyncer) SyncAll(context.Context) (int, int, error) {
	if c.calls.Add(1) == 1 && !c.closed.Swap(true) {
		close(c.fired)
	}
	return 0, 0, nil
}

// TestCatalogOpsSyncBeatDrivesTheSyncerOnATick is the mechanism-with-a-caller
// proof for #449's driver: the beat, on its interval, actually calls SyncAll — it
// is not a converge routine nobody schedules. Injected syncer + real ticker, the
// fire channel the synchronisation, context cancelled to stop the goroutine.
func TestCatalogOpsSyncBeatDrivesTheSyncerOnATick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &countingCatalogSyncer{fired: make(chan struct{})}
	runCatalogOpsSyncBeat(ctx, s, 2*time.Millisecond, slog.New(slog.DiscardHandler))

	select {
	case <-s.fired:
		// The beat called SyncAll — the caller exists.
	case <-time.After(5 * time.Second):
		t.Fatal("the catalog-ops sync beat never drove the syncer — it is a mechanism with no caller")
	}
	if s.calls.Load() < 1 {
		t.Fatalf("syncer was called %d times, want >= 1", s.calls.Load())
	}
}

// TestCatalogOpsSyncBeatRespectsAStoppedContext: a cancelled context means the
// runner never drives the syncer.
func TestCatalogOpsSyncBeatRespectsAStoppedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &countingCatalogSyncer{fired: make(chan struct{})}
	cancel()
	runCatalogOpsSyncBeat(ctx, s, time.Millisecond, slog.New(slog.DiscardHandler))
	time.Sleep(20 * time.Millisecond)
	if s.calls.Load() != 0 {
		t.Fatalf("a cancelled beat still drove the syncer %d times", s.calls.Load())
	}
}
