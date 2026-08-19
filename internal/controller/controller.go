// Package controller hosts the API, the job scheduler and the reconcilers. It
// owns coordinated mutable decisions and never routinely moves bulk content
// bytes (spec §7).
package controller

import (
	"context"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/config"
)

// Controller is the control-plane role.
type Controller struct {
	cfg config.Config
	log *slog.Logger
}

// New constructs the controller. It must not start anything; Run does that, so
// that construction failures are reported before any listener exists.
func New(cfg config.Config, log *slog.Logger) *Controller {
	return &Controller{cfg: cfg, log: log.With("role", "controller")}
}

// Name identifies the role in logs and supervision.
func (c *Controller) Name() string { return "controller" }

// Run blocks until ctx is cancelled, then shuts down cleanly.
//
// Milestone 1 fills this in: the JSON API (M1-14), the job scheduler (M1-05)
// and the reconcilers. Today it is the wiring only.
func (c *Controller) Run(ctx context.Context) error {
	c.log.Info("controller started",
		"database", c.cfg.Database.Path,
		"http_addr", c.cfg.HTTP.Addr,
		"auth_enabled", c.cfg.HTTP.Auth.Enabled)
	<-ctx.Done()
	c.log.Info("controller stopped")
	return nil
}
