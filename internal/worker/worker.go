// Package worker executes leased jobs. Workers own computation (spec §9, §75).
package worker

import (
	"context"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/config"
)

// Worker is the compute role.
type Worker struct {
	cfg config.Config
	log *slog.Logger
}

// New constructs the worker.
func New(cfg config.Config, log *slog.Logger) *Worker {
	return &Worker{cfg: cfg, log: log.With("role", "worker")}
}

// Name identifies the role in logs and supervision.
func (w *Worker) Name() string { return "worker" }

// Run blocks until ctx is cancelled, then drains.
//
// Milestone 1 fills this in: the lease loop, the handler registry and per-type
// concurrency caps (M1-09). Today it is the wiring only.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("worker started", "database", w.cfg.Database.Path)
	<-ctx.Done()
	// A real drain finishes in-flight jobs before returning; the supervisor's
	// shutdown grace period is what bounds it (M1-09).
	w.log.Info("worker stopped")
	return nil
}
