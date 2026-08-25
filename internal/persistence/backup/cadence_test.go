package backup_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
)

// TestRunCadenceTakesOnEachTick drives the loop by hand — one backup per tick,
// and a clean stop on cancellation — without a clock that advances (ADR-0017).
func TestRunCadenceTakesOnEachTick(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	took := make(chan struct{}, 4)
	take := func(context.Context) error {
		took <- struct{}{}
		return nil
	}
	done := make(chan struct{})
	go func() { backup.RunCadence(ctx, ticks, take, nil); close(done) }()

	ticks <- time.Now()
	waitFor(t, took, "the first tick did not produce a backup")
	ticks <- time.Now()
	waitFor(t, took, "the second tick did not produce a backup")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunCadence did not return after ctx was cancelled")
	}
}

// TestRunCadenceContinuesAfterError proves a failed backup does not stop the
// beat — the next tick still fires. A cadence that died on the first full disk
// would silently stop protecting the deployment.
func TestRunCadenceContinuesAfterError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	took := make(chan struct{}, 4)
	take := func(context.Context) error {
		took <- struct{}{}
		return errors.New("disk full")
	}
	go backup.RunCadence(ctx, ticks, take, nil)

	ticks <- time.Now()
	waitFor(t, took, "the first tick did not produce a backup attempt")
	ticks <- time.Now()
	waitFor(t, took, "the beat stopped after a failing backup instead of trying again")
}

func waitFor(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(msg)
	}
}
