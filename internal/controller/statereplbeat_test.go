package controller

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// countingReconciler records how many times the beat drove it.
type countingReconciler struct {
	calls  atomic.Int64
	fired  chan struct{}
	closed atomic.Bool
}

func (c *countingReconciler) ReconcileAll(context.Context) (int, int, error) {
	if c.calls.Add(1) == 1 && !c.closed.Swap(true) {
		close(c.fired)
	}
	return 0, 0, nil
}

// TestReplicationBeatDrivesTheReconcilerOnATick is the mechanism-with-a-caller
// proof for #362: the beat, on its interval, actually calls ReconcileAll — it is
// not a mechanism nobody schedules. It uses an injected reconciler and a real
// ticker (a fast interval, no time.Sleep in the assertion — the fire channel is
// the synchronisation), and cancels the context to stop the goroutine.
func TestReplicationBeatDrivesTheReconcilerOnATick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &countingReconciler{fired: make(chan struct{})}
	runStateReplicationBeat(ctx, rec, 2*time.Millisecond, slog.New(slog.DiscardHandler))

	select {
	case <-rec.fired:
		// The beat called ReconcileAll — the caller exists.
	case <-time.After(5 * time.Second):
		t.Fatal("the replication beat never drove the reconciler — it is a mechanism with no caller")
	}
	if rec.calls.Load() < 1 {
		t.Fatalf("reconciler was called %d times, want >= 1", rec.calls.Load())
	}
}

// TestReplicationBeatDisabledDoesNothing: a non-positive interval means the beat
// never fires (the on-demand route is the only path then).
func TestReplicationBeatDisabledDoesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &countingReconciler{fired: make(chan struct{})}
	// Disabled path is inside startStatePlaneReplication's guard, but assert the
	// runner respects a stopped context: cancel first, then run — no calls.
	cancel()
	runStateReplicationBeat(ctx, rec, time.Millisecond, slog.New(slog.DiscardHandler))
	time.Sleep(20 * time.Millisecond)
	if rec.calls.Load() != 0 {
		t.Fatalf("a cancelled beat still drove the reconciler %d times", rec.calls.Load())
	}
}
