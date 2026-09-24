// Package worker executes leased jobs. Workers own computation (spec §9, §75).
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/downloads"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/indexers"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media"
	"github.com/rarebit-one/heyarr-core/internal/media/capability"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/providers/musicbrainz"
	"github.com/rarebit-one/heyarr-core/internal/providers/openlibrary"
	"github.com/rarebit-one/heyarr-core/internal/providers/opensubtitles"
	"github.com/rarebit-one/heyarr-core/internal/providers/podcast"
	"github.com/rarebit-one/heyarr-core/internal/providers/tmdb"
	"github.com/rarebit-one/heyarr-core/internal/providers/tvdb"
	"github.com/rarebit-one/heyarr-core/internal/providers/webfeed"
	"github.com/rarebit-one/heyarr-core/internal/providers/youtube"
	"github.com/rarebit-one/heyarr-core/internal/scanner"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
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

// There is no minimum schema version to keep up to date here. A worker requires
// every migration compiled into its own binary to have been applied — the same
// set the controller from the same release applies (sqlite.UnappliedMigrations).
// A hand-maintained constant sat at 7 for forty-odd migrations, which made the
// guard below a guard against a database nobody has had since milestone 1; a
// worker started against a half-migrated database does not fail at startup, it
// fails on the first job, hours later, having already told the operator it was
// healthy.
//
// Upgrade order follows from it: the controller first, since it owns the schema
// (§7, ADR-0003). A worker from an older build than the database is fine —
// migrations it does not know about are not its requirement. A worker from a
// NEWER build than the controller waits for schemaWait and then refuses, naming
// what is missing, rather than running handlers against columns that are not
// there.

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

	// The toolchain is resolved before anything else touches the database, so a
	// misconfigured ffprobe path is a startup failure rather than something
	// discovered by the first probe job hours into a scan (ADR-0023). An
	// ABSENT toolchain is not a failure: this worker simply advertises fewer
	// capabilities and never claims the jobs that need them.
	toolchain, err := media.Resolve(startupCtx, media.Options{
		FFprobePath: w.cfg.Media.FFprobePath,
		FFmpegPath:  w.cfg.Media.FFmpegPath,
		Logger:      w.log,
	})
	if err != nil {
		return fmt.Errorf("worker: %w", err)
	}

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
	if err := waitForSchema(ctx, db, w.log, schemaWait); err != nil {
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

	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		return fmt.Errorf("worker: opening the job queue: %w", err)
	}

	integrityOpts := integrity.Options{
		Store: store, Catalog: cat, Logger: w.log,
		// ADR-0018's second precondition. Without it the gc_blobs job would
		// unlink bytes knowing nothing about whether they exist anywhere else
		// (M4-12) — and a nil here is a REFUSAL in any deployment with another
		// peer, never a pass.
		Durability: newLazyDurability(w.cfg.DataDir, peerID, db.Writer(), w.log),
	}
	checker, err := integrity.NewChecker(integrityOpts)
	if err != nil {
		return fmt.Errorf("worker: building the integrity checker: %w", err)
	}
	collector, err := integrity.NewCollector(integrityOpts)
	if err != nil {
		return fmt.Errorf("worker: building the garbage collector: %w", err)
	}

	// The scanner walks roots and enqueues ingest work; it never reads a file
	// it has already seen unchanged (M1-12). It runs here rather than in the
	// controller because walking a 4 TB library is computation, and computation
	// belongs to workers (§9).
	scan, err := scanner.New(scanner.Options{
		Store:  cat,
		Queue:  queue,
		Logger: w.log,
	})
	if err != nil {
		return fmt.Errorf("worker: building the scanner: %w", err)
	}

	// The provider registry, built from configuration (§59, M3-07). It is built
	// before the registrations: the provider-routed handlers are registered only
	// for the capabilities it has.
	//
	// Validation already happened at config load, so a malformed endpoint or a
	// missing credential stopped this process before it opened a database. What
	// remains is construction, which cannot fail for a reason an operator can
	// act on.
	resolvedProviders, err := providers.Validate(w.cfg.Providers)
	if err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	providerRegistry, err := providers.BuildWith(resolvedProviders, w.log, nil,
		providers.Chain(indexers.Constructor, downloads.Constructor, tvdb.Constructor, tmdb.Constructor, podcast.Constructor, youtube.Constructor, webfeed.Constructor, opensubtitles.Constructor, musicbrainz.Constructor, openlibrary.Constructor))
	if err != nil {
		return fmt.Errorf("worker: building the provider registry: %w", err)
	}

	deps := workerDeps{
		cfg: w.cfg, log: w.log, db: db, store: store, cat: cat, queue: queue,
		pipeline: pipeline, checker: checker, collector: collector, scan: scan,
		peerID: peerID, providers: providerRegistry, toolchain: toolchain,
	}
	registry := NewRegistry()
	registerStorageJobs(registry, deps)
	registerAcquisitionJobs(registry, deps)
	registerReplicationJobs(registry, deps)
	registerProviderJobs(registry, deps)
	if err := registerMediaJobs(registry, deps); err != nil {
		return err
	}

	workerID := owner()
	startedAt := time.Now().UTC()
	runtime, err := NewRuntime(Config{
		Owner: workerID,
		// What this worker can do, not what it would like to. A job requiring
		// a capability nobody advertises stays pending and visible rather than
		// failing (§75, ADR-0023).
		//
		// Two vocabularies meet here and it is the only place they do: the
		// media toolchain contributes what BINARIES resolved, the provider
		// registry contributes what SERVICES are configured. Both answer "what
		// can this node execute", which is what the job queue matches on.
		Capabilities: append(toolchain.Capabilities(), providerRegistry.JobCapabilities()...),
	}, queue, registry, w.log)
	if err != nil {
		return fmt.Errorf("worker: building the runtime: %w", err)
	}

	// The capability advertisement beat (ADR-0039, M5-112).
	//
	// It runs even on a node with no toolchain and no providers, and that is
	// deliberate: an advertisement of NOTHING is the answer to "why is nothing
	// transcoding", and a worker that stayed silent because it had nothing to
	// say would be indistinguishable from one that had died. It is also the
	// only thing that renews this worker's row, so a beat that stood down would
	// let a healthy worker expire.
	//
	// The hardware runner exists only where there is a binary to run: ADR-0023
	// resolves it at startup and does not re-resolve it, so a node without
	// ffmpeg has nothing to exercise for as long as this process lives.
	var runner capability.Runner
	if toolchain.FFmpeg.Available {
		r, err := capability.NewExecRunner(toolchain.FFmpeg.Path)
		if err != nil {
			return fmt.Errorf("worker: building the capability prober: %w", err)
		}
		runner = r
	}
	beat, err := NewCapabilityBeat(AdvertiserOptions{
		WorkerID: workerID,
		PeerID:   peerID,
		PeerName: w.cfg.Peer.Name,
		// The binary and service halves, captured. Both are startup facts by
		// ADR-0023 and ADR-0025, and neither is re-resolved by the beat — which
		// is the asymmetry with the hardware probe below.
		Binary: capability.Merge(
			BinaryCapabilities(toolchain.Capabilities(), startedAt),
			ServiceCapabilities(providerRegistry.JobCapabilities(), startedAt),
		),
		Runner:     runner,
		Candidates: capability.DefaultCandidates(),
		Recorder:   cat,
		Logger:     w.log,
	})
	if err != nil {
		return fmt.Errorf("worker: building the capability beat: %w", err)
	}

	if ctx.Err() != nil {
		w.log.Info("worker stopped during startup")
		return nil
	}

	go beat.Run(ctx)

	w.log.Info("worker ready", "cas_root", store.Root(), "peer_id", peerID)
	// Runtime.Run logs its slots, capabilities and registered job types, and
	// drains in-flight jobs on cancellation.
	if err := runtime.Run(ctx); err != nil {
		return err
	}
	w.log.Info("worker stopped")
	return nil
}

// waitForSchema blocks until the controller has applied every migration this
// worker's binary knows about, polling for the condition rather than sleeping.
func waitForSchema(ctx context.Context, db *sqlite.DB, log *slog.Logger, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	last := -1
	for {
		missing, err := sqlite.UnappliedMigrations(ctx, db)
		if err == nil && len(missing) == 0 {
			return nil
		}
		if err == nil && len(missing) != last {
			log.Info("waiting for the controller to migrate the schema",
				"unapplied", len(missing), "first", missing[0], "last", missing[len(missing)-1])
			last = len(missing)
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("worker: schema was not ready within %s: %w", wait, err)
			}
			return fmt.Errorf("worker: after %s the database is still missing %d migration(s) this worker needs "+
				"(%s) — is a controller from this release or newer running against %s?",
				wait, len(missing), describeVersions(missing), db.Path())
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("worker: waiting for the schema: %w", ctx.Err())
		case <-time.After(schemaPollInterval):
		}
	}
}

// describeVersions renders a list of migration versions for an error message,
// eliding the middle of a long one: "00041, 00042, ... 00054 (14)".
func describeVersions(versions []int64) string {
	format := func(vs []int64) string {
		parts := make([]string, len(vs))
		for i, v := range vs {
			parts[i] = fmt.Sprintf("%05d", v)
		}
		return strings.Join(parts, ", ")
	}
	if len(versions) <= 5 {
		return format(versions)
	}
	return fmt.Sprintf("%s, ... %s (%d)", format(versions[:2]), format(versions[len(versions)-1:]), len(versions))
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
