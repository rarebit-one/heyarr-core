// Package controller hosts the API, the job scheduler and the reconcilers. It
// owns coordinated mutable decisions and never routinely moves bulk content
// bytes (spec §7).
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Controller is the control-plane role.
type Controller struct {
	cfg config.Config
	log *slog.Logger
	db  *sqlite.DB
}

// New constructs the controller. It must not start anything; Run does that, so
// that construction failures are reported before any listener exists.
func New(cfg config.Config, log *slog.Logger) *Controller {
	return &Controller{cfg: cfg, log: log.With("role", "controller")}
}

// Name identifies the role in logs and supervision.
func (c *Controller) Name() string { return "controller" }

// startupTimeout bounds the work that must finish even if shutdown is
// requested — opening the database and migrating it. It exists so that a
// wedged migration cannot make the process unkillable.
const startupTimeout = 5 * time.Minute

// Run blocks until ctx is cancelled, then shuts down cleanly.
//
// Milestone 1 fills this in: the JSON API (M1-14), the job scheduler (M1-05)
// and the reconcilers. Today it is the wiring only.
func (c *Controller) Run(ctx context.Context) error {
	// Startup deliberately does NOT use the shutdown context.
	//
	// A SIGTERM arriving while a migration is in flight would otherwise cancel
	// it mid-statement. Transactional DDL means that is safe rather than
	// corrupting, but it turns an ordinary restart into a startup *error* and
	// makes the next start redo the work — and a service you are afraid to
	// restart during an upgrade is worse than one that takes a moment longer to
	// stop. So schema work runs to completion, bounded by a timeout so a wedged
	// migration cannot make the process unkillable.
	startupCtx, cancelStartup := context.WithTimeout(context.WithoutCancel(ctx), startupTimeout)
	defer cancelStartup()

	// The controller is the only role that opens the database for writing: it
	// owns coordinated mutable state (§7, ADR-0003). Workers and peers reach it
	// through the controller, never by opening the file themselves.
	db, err := sqlite.Open(startupCtx, sqlite.Options{Path: c.cfg.Database.Path, Logger: c.log})
	if err != nil {
		return fmt.Errorf("controller: opening database: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			c.log.Error("closing database", "error", err)
		}
	}()

	if err := sqlite.Migrate(startupCtx, db); err != nil {
		return fmt.Errorf("controller: migrating database: %w", err)
	}
	version, err := sqlite.SchemaVersion(startupCtx, db)
	if err != nil {
		return fmt.Errorf("controller: reading schema version: %w", err)
	}

	// Shutdown may have been requested while the schema work ran. That is a
	// clean stop, not a failure — report it as one.
	if ctx.Err() != nil {
		c.log.Info("controller stopped during startup", "schema_version", version)
		return nil
	}

	c.db = db
	c.log.Info("controller started",
		"database", c.cfg.Database.Path,
		"schema_version", version,
		"http_addr", c.cfg.HTTP.Addr,
		"auth_enabled", c.cfg.HTTP.Auth.Enabled)
	<-ctx.Done()
	c.log.Info("controller stopped")
	return nil
}
