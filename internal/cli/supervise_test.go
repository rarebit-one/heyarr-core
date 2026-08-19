package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRole records what the supervisor did to it.
type fakeRole struct {
	name    string
	started atomic.Bool
	stopped atomic.Bool
	err     error
	// block, when non-nil, is waited on instead of the context — used to
	// simulate a role that ignores cancellation.
	block chan struct{}
}

func (f *fakeRole) Name() string { return f.name }

func (f *fakeRole) Run(ctx context.Context) error {
	f.started.Store(true)
	if f.err != nil {
		return f.err
	}
	if f.block != nil {
		<-f.block
	} else {
		<-ctx.Done()
	}
	f.stopped.Store(true)
	return nil
}

func TestSuperviseRunsEveryRoleAndStopsThemAll(t *testing.T) {
	a := &fakeRole{name: "a"}
	b := &fakeRole{name: "b"}
	c := &fakeRole{name: "c"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervise(ctx, discardLogger(), time.Second, a, b, c) }()

	waitFor(t, func() bool { return a.started.Load() && b.started.Load() && c.started.Load() })
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("supervise returned %v, want nil", err)
	}
	for _, r := range []*fakeRole{a, b, c} {
		if !r.stopped.Load() {
			t.Errorf("role %s was not stopped", r.name)
		}
	}
}

// One role exiting must take the whole process down. A peer serving bytes with
// no controller to authorise them is a worse state than a stopped process.
func TestSuperviseOneFailureStopsTheRest(t *testing.T) {
	boom := errors.New("boom")
	failing := &fakeRole{name: "controller", err: boom}
	healthy := &fakeRole{name: "peer"}

	err := supervise(context.Background(), discardLogger(), time.Second, failing, healthy)
	if !errors.Is(err, boom) {
		t.Fatalf("supervise returned %v, want it to wrap %v", err, boom)
	}
	if !strings.Contains(err.Error(), "controller") {
		t.Errorf("error = %q, want it to name the role that failed", err)
	}
	if !healthy.stopped.Load() {
		t.Error("the healthy role was not stopped when its sibling failed")
	}
}

// A supervisor that hangs forever on shutdown is worse than one that gives up
// loudly: systemd SIGKILLs the process anyway and then nothing explains why.
func TestSuperviseReportsRolesThatOverrunTheGracePeriod(t *testing.T) {
	stuck := &fakeRole{name: "worker", block: make(chan struct{})}
	t.Cleanup(func() { close(stuck.block) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervise(ctx, discardLogger(), 50*time.Millisecond, stuck) }()

	waitFor(t, func() bool { return stuck.started.Load() })
	cancel()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "did not stop within") {
			t.Fatalf("supervise returned %v, want a grace-period error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervise hung past the grace period instead of reporting")
	}
}

func TestSuperviseRejectsNoRoles(t *testing.T) {
	if err := supervise(context.Background(), discardLogger(), time.Second); err == nil {
		t.Error("supervise accepted an empty role set")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
