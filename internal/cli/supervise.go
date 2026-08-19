package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Role is one independently runnable unit of Heyarr — controller, worker or
// peer. Run blocks until its context is cancelled and then returns.
//
// ADR-0002: roles must communicate only through the controller database and
// HTTP, never a shared in-process pointer. This interface is deliberately
// narrow so that `heyarr all` cannot hand one role a reference to another; if a
// call could not survive becoming a network hop, it does not belong here.
type Role interface {
	Name() string
	Run(ctx context.Context) error
}

// DefaultShutdownGrace bounds how long roles may take to stop once cancelled.
// A worker draining an in-flight job is the case this exists for.
const DefaultShutdownGrace = 15 * time.Second

// supervise runs every role concurrently until ctx is cancelled or one of them
// returns an error, then cancels the rest and waits for them, bounded by grace.
//
// The first non-nil error is returned. A role that has not returned within the
// grace period is reported rather than waited on forever, because a supervisor
// that hangs on shutdown is worse than one that gives up loudly: systemd will
// SIGKILL the process anyway, and then nothing explains why.
func supervise(ctx context.Context, log *slog.Logger, grace time.Duration, roles ...Role) error {
	if len(roles) == 0 {
		return errors.New("supervise: no roles to run")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, r := range roles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.Run(ctx)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", r.Name(), err))
				mu.Unlock()
				log.Error("role failed", "role", r.Name(), "error", err)
			}
			// One role exiting takes the process down. A half-running Heyarr —
			// a peer serving bytes with no controller to authorise them — is a
			// worse state than a stopped one, and systemd should restart it.
			cancel()
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	<-ctx.Done()

	select {
	case <-done:
	case <-time.After(grace):
		mu.Lock()
		errs = append(errs, fmt.Errorf("supervise: roles did not stop within %s", grace))
		mu.Unlock()
		log.Error("shutdown grace period expired", "grace", grace)
	}

	mu.Lock()
	defer mu.Unlock()
	return errors.Join(errs...)
}
