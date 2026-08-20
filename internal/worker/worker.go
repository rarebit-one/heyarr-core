// Package worker executes leased jobs. Workers own computation (spec §9, §75).
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
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

// minSchemaVersion is the migration this role's handlers require. A worker that
// starts against an older schema does not fail at startup — it fails on the
// first job, hours later, having already told the operator it was healthy.
const minSchemaVersion = 5

// schemaWait bounds how long the worker waits for the controller to migrate.
// The roles start concurrently (ADR-0002) and the controller is the slow one
// precisely because it migrates, so some wait is normal and an unbounded one is
// a hang nobody can diagnose.
const schemaWait = 2 * time.Minute

// schemaPollInterval is how often the wait re-checks. Polling for the condition
// rather than sleeping a fixed duration: a fixed wait is a bet on machine
// speed, and every one of those bets in this repo has eventually lost on CI.
const schemaPollInterval = 100 * time.Millisecond

// Run claims and executes jobs until ctx is cancelled, then drains.
//
// Started and ready are two different things here, and the log says so. A
// worker started before any controller has ever run is legitimately alive and
// legitimately unable to do anything: roles are independently runnable as OS
// processes (ADR-0002) and start concurrently, so waiting for the schema is an
// ordinary startup state rather than a fault. It reports "worker started" when
// it is alive and supervised, and "worker ready" when it can actually claim
// work.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("worker started", "database", w.cfg.Database.Path)

	// Startup does not use the shutdown context, for the same reason the
	// controller's does not: a SIGTERM arriving mid-startup should be a clean
	// stop, not a startup error that the next start has to redo.
	startupCtx, cancelStartup := context.WithTimeout(context.WithoutCancel(ctx), schemaWait+time.Minute)
	defer cancelStartup()

	db, err := sqlite.Open(startupCtx, sqlite.Options{Path: w.cfg.Database.Path, Logger: w.log})
	if err != nil {
		return fmt.Errorf("worker: opening database: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			w.log.Error("closing database", "error", err)
		}
	}()

	// The worker does NOT migrate, and does not ask goose anything either. The
	// controller owns the schema (§7, ADR-0003), and even the question "what
	// version are you at?" is a write when goose answers it — see
	// sqlite.AppliedSchemaVersion.
	//
	// Unlike opening the database, WAITING is interruptible: a SIGTERM arriving
	// while the controller is still migrating should stop this process, not
	// leave it polling for two minutes past the point anyone wanted it alive.
	if err := waitForSchema(ctx, db, w.log); err != nil {
		if ctx.Err() != nil {
			w.log.Info("worker stopped while waiting for the schema")
			return nil
		}
		return err
	}

	store, err := cas.OpenFS(w.cfg.CAS.Root)
	if err != nil {
		return fmt.Errorf("worker: opening the content store: %w", err)
	}

	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Logger: w.log})
	if err != nil {
		return fmt.Errorf("worker: opening the event log: %w", err)
	}

	cat, err := catalog.New(catalog.Options{
		DB:       db,
		Events:   eventLog,
		PeerName: w.cfg.Peer.Name,
		PeerSite: w.cfg.Peer.Site,
		Logger:   w.log,
	})
	if err != nil {
		return fmt.Errorf("worker: opening the catalog: %w", err)
	}
	// Resolve the self peer now rather than on the first ingest, so a
	// misconfigured peer name is a startup failure rather than a job failure
	// (ADR-0010).
	peerID, err := cat.SelfPeer(startupCtx)
	if err != nil {
		return fmt.Errorf("worker: resolving this peer: %w", err)
	}

	pipeline, err := ingest.New(ingest.Options{
		Store:      NewCASByteStore(store),
		Catalog:    cat,
		Identifier: identification.Default(),
		Logger:     w.log,
	})
	if err != nil {
		return fmt.Errorf("worker: building the ingest pipeline: %w", err)
	}

	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		return fmt.Errorf("worker: opening the job queue: %w", err)
	}

	registry := NewRegistry()
	registry.RegisterFunc(ingest.JobType, IngestHandler(pipeline))

	runtime, err := NewRuntime(Config{Owner: owner()}, queue, registry, w.log)
	if err != nil {
		return fmt.Errorf("worker: building the runtime: %w", err)
	}

	if ctx.Err() != nil {
		w.log.Info("worker stopped during startup")
		return nil
	}

	w.log.Info("worker ready", "cas_root", store.Root(), "peer_id", peerID)
	// Runtime.Run logs its slots, capabilities and registered job types, and
	// drains in-flight jobs on cancellation.
	if err := runtime.Run(ctx); err != nil {
		return err
	}
	w.log.Info("worker stopped")
	return nil
}

// waitForSchema blocks until the controller has migrated far enough for this
// worker's handlers, polling for the condition rather than sleeping.
func waitForSchema(ctx context.Context, db *sqlite.DB, log *slog.Logger) error {
	deadline := time.Now().Add(schemaWait)
	var last int64 = -1
	for {
		version, err := sqlite.AppliedSchemaVersion(ctx, db)
		if err == nil && version >= minSchemaVersion {
			return nil
		}
		if err == nil && version != last {
			log.Info("waiting for the controller to migrate the schema",
				"have", version, "need", minSchemaVersion)
			last = version
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("worker: schema did not reach version %d within %s: %w",
					minSchemaVersion, schemaWait, err)
			}
			return fmt.Errorf("worker: schema is at version %d after %s, this worker needs %d — "+
				"is a controller running against %s?", last, schemaWait, minSchemaVersion, db.Path())
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("worker: waiting for the schema: %w", ctx.Err())
		case <-time.After(schemaPollInterval):
		}
	}
}

// owner identifies this worker in leases. It must be unique per process: two
// workers sharing an owner can renew each other's leases, which is a way to run
// the same job twice with nothing in the log to say so.
func owner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), uuid.Must(uuid.NewV7()).String()[:8])
}
