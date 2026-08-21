// Package controller hosts the API, the job scheduler and the reconcilers. It
// owns coordinated mutable decisions and never routinely moves bulk content
// bytes (spec §7).
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
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

// shutdownTimeout bounds draining in-flight HTTP requests. It sits inside
// cli.DefaultShutdownGrace so that the server gives up before the supervisor
// does — otherwise the supervisor's message ("roles did not stop") replaces the
// server's, which is the one that says what was still in flight.
const shutdownTimeout = 10 * time.Second

// Run blocks until ctx is cancelled, then shuts down cleanly.
//
// Milestone 1 fills this in: the resource API (M1-14), the job scheduler
// (M1-05) and the reconcilers mount onto the HTTP foundation started here.
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

	// The libraries block is control-plane configuration, so the controller
	// owns turning it into rows — and the scan that follows is a job, because
	// the worker that runs it may be another process entirely (§4, ADR-0002).
	//
	// Like the migration above it, this runs on the STARTUP context rather than
	// the shutdown one, and for the same reason: it is idempotent schema-shaped
	// work that the next start would only have to redo, and a SIGTERM arriving
	// here should be an ordinary stop rather than a start that half-happened.
	// A misconfigured library is a startup failure — discovering it hours later
	// when a scan silently never happens is much more expensive.
	if err := reconcileLibraries(startupCtx, db, c.cfg, c.log); err != nil {
		return err
	}

	// Quality profiles are seeded before anything else can want them (§62,
	// M3-01). A Heyarr with no profiles is one where the first interesting
	// thing you can do — want something — requires authoring JSON against a
	// vocabulary you have not read.
	//
	// It converges on the profile name and never overwrites, so an operator
	// who tunes a default keeps their tuning across every restart.
	if err := seedQualityProfiles(startupCtx, db, c.cfg, c.log); err != nil {
		return err
	}

	// Shutdown may have been requested while the schema work ran. That is a
	// clean stop, not a failure — report it as one.
	if ctx.Err() != nil {
		c.log.Info("controller stopped during startup", "schema_version", version)
		return nil
	}

	c.db = db

	// The CAS root is the controller's to have ready before it says it is up:
	// /readyz reports on it, and an operator who has not created it yet should
	// learn that from a readiness probe rather than from an ingest failing
	// hours into a scan. The layout inside it belongs to the storage fabric
	// (ADR-0006); all that happens here is that the directory exists.
	if c.cfg.CAS.Root != "" {
		if err := os.MkdirAll(c.cfg.CAS.Root, 0o750); err != nil {
			return fmt.Errorf("controller: creating the CAS root %s: %w", c.cfg.CAS.Root, err)
		}
	}

	// The CAS is opened once, here, and handed to whatever serves from it. The
	// controller does not know the layout inside the root (ADR-0006) — only
	// that a store exists to read blobs through.
	blobStore, err := cas.OpenFS(c.cfg.CAS.Root)
	if err != nil {
		return fmt.Errorf("controller: opening the CAS at %s: %w", c.cfg.CAS.Root, err)
	}

	srv, err := c.newServer(db, blobStore, version)
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("controller: %w", err)
	}

	// Reconciliation runs on the SERVING context, not the startup one: it is
	// ongoing work rather than schema-shaped setup, and it must stop when the
	// controller does.
	reconcileEvents, err := events.New(events.Options{
		Writer: db.Writer(), Reader: db.Reader(), Logger: c.log,
	})
	if err != nil {
		return fmt.Errorf("controller: opening the event log for reconciliation: %w", err)
	}
	reconcileQueue, err := jobs.New(jobs.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: reconcileEvents,
	})
	if err != nil {
		return fmt.Errorf("controller: opening the job queue for reconciliation: %w", err)
	}
	startReconciliation(ctx, reconcileQueue, c.log)

	// "started" is logged only after every listener is bound. A start line
	// printed before the socket exists is a lie that costs someone an
	// afternoon: the supervisor, the acceptance script and an operator tailing
	// the log all treat it as "you can talk to it now".
	c.log.Info("controller started",
		"database", c.cfg.Database.Path,
		"schema_version", version,
		"http_addr", srv.Addr(),
		"unix_socket", srv.SocketPath(),
		"auth_enabled", c.cfg.HTTP.Auth.Enabled)

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-srv.Err():
		runErr = err
	}

	// Draining must not use the cancelled context, or shutdown would return
	// instantly and kill every in-flight request — including a range response
	// halfway through a large blob, which is precisely the request most worth
	// finishing.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		c.log.Error("the http server did not shut down cleanly", "error", err)
	}

	c.log.Info("controller stopped")
	return runErr
}

// newServer wires the HTTP API. Authentication is constructed even when it is
// disabled, so the two configurations differ in one boolean rather than in
// which objects exist.
func (c *Controller) newServer(db *sqlite.DB, blobStore cas.Store, schemaVersion int64) (*httpapi.Server, error) {
	store, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{Store: store})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	// Resolved here so a misconfigured path stops startup rather than being
	// discovered later, and so GET /api/v1/system can report what this node
	// can actually do (ADR-0023). An absent toolchain is not an error.
	toolchain, err := media.Resolve(context.Background(), media.Options{
		FFprobePath: c.cfg.Media.FFprobePath,
		FFmpegPath:  c.cfg.Media.FFmpegPath,
		Logger:      c.log,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}

	// The log is built here rather than inside mounts because it now has two
	// consumers: the resource API, which appends to it, and GET /api/v1/system,
	// which reports its head. One log, so the head the endpoint reports is the
	// head the stream resumes from.
	eventLog, err := events.New(events.Options{
		Writer: db.Writer(), Reader: db.Reader(), Logger: c.log,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	mounts, err := c.mounts(db, store, blobStore, eventLog)
	if err != nil {
		return nil, err
	}
	srv, err := httpapi.New(httpapi.Options{
		Config:        c.cfg,
		Logger:        c.log,
		DB:            db,
		Verifier:      verifier,
		Events:        eventLog,
		Media:         mediaInfo(toolchain),
		Build:         buildinfo.Get(),
		SchemaVersion: schemaVersion,
		CASRoot:       c.cfg.CAS.Root,
		Mount:         mounts,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	return srv, nil
}

// mounts is the API surface this controller serves.
//
// It is one function rather than a literal inside newServer because the
// OpenAPI parity test (ADR-0015) drives *this* list. A test that built its own
// list of mounts would be asserting that the specification matches a second
// hand-maintained list, which is the failure the ADR exists to prevent.
// The event log is passed in rather than built here because the queue records
// its own transitions through it (§76, ADR-0009) and GET /api/v1/system reports
// its head — and those must be the same log.
func (c *Controller) mounts(db *sqlite.DB, store *auth.Store, blobStore cas.Store, eventLog *events.Log) ([]httpapi.MountFunc, error) {
	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	// The catalog answers §56's satisfaction questions for the API. It is the
	// same construction reconcileLibraries and the worker use — one catalog
	// per process would be tidier, and is a refactor rather than this issue.
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog,
		PeerName: c.cfg.Peer.Name, PeerSite: c.cfg.Peer.Site, Logger: c.log,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: opening the catalog: %w", err)
	}
	api, err := resources.New(resources.Options{
		DB:      db,
		Jobs:    queue,
		Events:  eventLog,
		Tokens:  store,
		Catalog: cat,
		Logger:  c.log,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	blobHandler, err := blobs.New(blobs.Options{Store: blobStore, Logger: c.log})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	return []httpapi.MountFunc{api.Mount, blobHandler.Mount}, nil
}

// mediaInfo renders a resolved toolchain for GET /api/v1/system.
//
// The mapping lives here rather than in internal/media so that the media
// package stays free of the API's wire shape, and in the controller rather
// than in the HTTP package so that the HTTP foundation does not import the
// toolchain to describe it.
func mediaInfo(tc media.Toolchain) []httpapi.ToolInfo {
	tools := tc.Tools()
	out := make([]httpapi.ToolInfo, 0, len(tools))
	for _, t := range tools {
		out = append(out, httpapi.ToolInfo{
			Name:      t.Name,
			Path:      t.Path,
			Version:   t.Version,
			Available: t.Available,
			Detail:    t.Detail,
		})
	}
	return out
}
