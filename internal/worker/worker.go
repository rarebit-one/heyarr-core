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

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/downloads"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/indexers"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/providers"
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

// minSchemaVersion is the migration this role's handlers require. A worker that
// starts against an older schema does not fail at startup — it fails on the
// first job, hours later, having already told the operator it was healthy.
const minSchemaVersion = 7

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

	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		return fmt.Errorf("worker: opening the job queue: %w", err)
	}

	integrityOpts := integrity.Options{Store: store, Catalog: cat, Logger: w.log}
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

	registry := NewRegistry()
	registry.RegisterFunc(ingest.JobType, IngestHandler(pipeline, queue))
	registry.RegisterFunc(scanner.JobType, ScanHandler(scan))
	registry.RegisterFunc(integrity.VerifyJobType, VerifyBlobHandler(checker, w.log))
	// One garbage collection at a time. Two concurrent sweeps would each walk
	// the store while the other unlinked from it, and the loser would spend the
	// pass reporting the winner's deletions as missing blobs.
	registry.Register(integrity.GCJobType, Registration{
		Handler:       GCHandler(collector, w.log),
		MaxConcurrent: 1,
	})
	// Reconciliation answers §56's two questions for every want (§57, M3-05).
	//
	// One at a time, for the same reason as garbage collection: two concurrent
	// sweeps would each read the library while the other wrote its
	// conclusions, and the loser would spend the pass recording answers that
	// were already stale.
	//
	// No RequiredCapability. It needs nothing but the database — no toolchain,
	// no indexer, no download client — so a fully degraded node still knows
	// what it is missing, which is exactly the node whose operator most needs
	// to be told.
	registry.Register(acquisition.ReconcileJobType, Registration{
		Handler:       ReconcileHandler(cat, w.log),
		MaxConcurrent: 1,
	})
	// The upgrade scan (§60, M3-06). One at a time, for the same reason
	// reconciliation is: two concurrent scans would each read the library
	// while the other concluded.
	//
	// No RequiredCapability. It reads the database and decides; it needs no
	// toolchain, no indexer and no download client — and a node that cannot
	// acquire anything can still tell an operator what could be better, which
	// is exactly the node whose operator most wants to know.
	registry.Register(acquisition.UpgradeScanJobType, Registration{
		Handler:       UpgradeScanHandler(cat, w.log),
		MaxConcurrent: 1,
	})
	// Peer convergence (§19, §57, M4-08). §19's desired blob set against what
	// the peers report holding, emitting replicate_blob for the difference.
	//
	// One at a time, for the reason the two sweeps above are: two concurrent
	// cycles would each read the fabric while the other enqueued against it,
	// and the loser would spend the pass deciding against a picture that had
	// already moved. The dedupe key keeps the RESULT correct either way — it
	// is a unique index, not a convention — but the second cycle would still
	// be wasted work whose counts described a fabric nobody saw.
	//
	// No RequiredCapability, following the precedent above. It needs nothing
	// but the database: no toolchain, no indexer, no download client, and not
	// even a reachable peer — the diff is against the last inventory a peer
	// reported, not against a live probe. A fully degraded node still knows
	// what it is missing, which is exactly the node whose operator most needs
	// to be told.
	registry.Register(replication.ReconcilePeerJobType,
		ReconcilePeerRegistration(cat, queue, w.log))

	// The provider registry, built from configuration (§59, M3-07).
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
		providers.Chain(indexers.Constructor, downloads.Constructor))
	if err != nil {
		return fmt.Errorf("worker: building the provider registry: %w", err)
	}

	// The health pass. One at a time, and no RequiredCapability: a node with
	// NO providers still runs it, finds nothing, and does nothing — which is
	// cheaper than a capability check and means the job is never mysteriously
	// pending on a node that simply has nothing to check.
	registry.Register(providers.HealthJobType, Registration{
		Handler:       ProviderHealthHandler(providerRegistry, cat, w.log),
		MaxConcurrent: 1,
	})

	// Capability routing's second and third users, after the media toolchain
	// (ADR-0023). A node with no indexer configured advertises no `indexer`
	// capability, so a search job stays PENDING AND VISIBLE rather than being
	// claimed and failed — which is ADR-0025's whole claim.
	//
	// The search handler, registered only when this worker has an indexer —
	// the same reasoning as the probe and poll handlers. Not registering it at
	// all makes the degraded state visible in the startup log, which lists the
	// types this worker will claim, and "why is nothing being searched for"
	// should be answerable from that log.
	//
	// On a node with no indexer the job stays PENDING AND VISIBLE rather than
	// being claimed and failed, which is ADR-0025's whole claim: a search that
	// cannot run is work waiting for a capability, not work that went wrong.
	//
	// MaxConcurrent is deliberately NOT 1. Unlike reconciliation and the
	// upgrade scan, each search is scoped to one want and writes only that
	// want's rows, so two running at once contend over nothing — and a library
	// of two hundred wants would otherwise take two hundred sequential
	// provider round trips to make one pass.
	if providerRegistry.Has(providers.CapabilityIndexer) {
		w.log.Info("indexing is available",
			"providers", strings.Join(indexerNames(providerRegistry), ", "))
		registry.Register(acquisition.SearchJobType, Registration{
			Handler:            SearchHandler(providerRegistry, cat, w.log),
			RequiredCapability: providers.CapabilityIndexer.JobCapability(),
		})
	}
	// The poll handler, registered only when this worker has a download client
	// — the same reasoning as the probe handler below. Not registering it at
	// all makes the degraded state visible in the startup log, which lists the
	// types this worker will claim, and "why is nothing being acquired" should
	// be answerable from that log.
	//
	// One at a time: two concurrent passes would each read a client's queue
	// while the other wrote its conclusions, and the loser would record
	// progress that was already stale.
	if providerRegistry.Has(providers.CapabilityDownload) {
		w.log.Info("a download client is available",
			"providers", strings.Join(downloadClientNames(providerRegistry), ", "))
		registry.Register(downloads.PollJobType, Registration{
			Handler:            PollDownloadsHandler(providerRegistry, cat, queue, w.log),
			MaxConcurrent:      1,
			RequiredCapability: providers.CapabilityDownload.JobCapability(),
		})
	}

	// Ingest of completed acquisitions (§65, M3-13).
	//
	// Registered UNCONDITIONALLY, unlike the poll above, and the asymmetry is
	// deliberate. Polling needs a download client; hashing a file that is
	// already on disk needs nothing but the disk. A node with no download
	// client configured can still finish an acquisition another node started —
	// which is what a compute peer is for (§6) — and refusing to register the
	// handler would make that impossible for no reason.
	//
	// One at a time: hashing is I/O-bound on the same storage the CAS writes
	// to, and running several against one disk is slower than running one.
	registry.Register(acquisition.IngestJobType, Registration{
		Handler:       IngestAcquisitionHandler(cat, cat, pipeline, queue, w.log),
		MaxConcurrent: 1,
	})

	// The probe handler, registered only when this worker can actually run it.
	//
	// Registering it unconditionally with RequiredCapability set would also
	// work — claimableTypes() filters on the capability — but not registering
	// it at all makes the degraded state visible in the startup log, which
	// lists the types this worker will claim. "Why is nothing probing" should
	// be answerable from the log a worker prints when it starts.
	if toolchain.FFprobe.Available {
		prober, err := probe.New(probe.Options{
			FFprobePath: toolchain.FFprobe.Path,
			TempDir:     w.cfg.DataDir,
			Logger:      w.log,
		})
		if err != nil {
			return fmt.Errorf("worker: building the prober: %w", err)
		}
		endpoint := w.cfg.PeerEndpoint()
		client, baseURL, err := probe.EndpointClient(endpoint, 30*time.Second)
		if err != nil {
			// A node that can probe but cannot reach itself is a
			// misconfiguration worth stopping for: the alternative is every
			// probe job failing at runtime with the same error, five times
			// each, forever.
			return fmt.Errorf("worker: %w", err)
		}
		prober.SetHTTPClient(client)

		authStore, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader()})
		if err != nil {
			return fmt.Errorf("worker: opening the credential store for probes: %w", err)
		}
		registry.Register(probe.JobType, Registration{
			RequiredCapability: probe.Capability,
			Handler: ProbeHandler(ProbeHandlerOptions{
				Prober: prober, Recorder: cat, Tokens: authStore,
				BaseURL: baseURL, Logger: w.log,
			}),
			// Probes are subprocesses that read over the network. Two at once
			// is fine; twenty is a worker that has stopped doing anything else
			// and a peer serving twenty concurrent range storms.
			MaxConcurrent: 2,
		})
		w.log.Info("probing is available", "endpoint", endpoint, "ffprobe", toolchain.FFprobe.Version)
	}

	// The remux handler, registered only when this worker can run it — same
	// reasoning as the prober: not registering it makes the degraded state
	// visible in the startup log, which lists the types this worker claims.
	if toolchain.FFmpeg.Available {
		remuxer, err := ffmpeg.New(ffmpeg.Options{
			FFmpegPath: toolchain.FFmpeg.Path,
			// Inside the data directory, so the output shares a filesystem
			// with the store and adoption is metadata rather than a copy
			// (ADR-0014).
			WorkDir: w.cfg.DataDir,
			Logger:  w.log,
		})
		if err != nil {
			return fmt.Errorf("worker: building the remuxer: %w", err)
		}
		registry.Register(ffmpeg.JobType, Registration{
			RequiredCapability: ffmpeg.Capability,
			Handler: RemuxHandler(RemuxHandlerOptions{
				Remuxer:  remuxer,
				Store:    NewCASRemuxStore(store),
				Recorder: cat,
				Logger:   w.log,
			}),
			// One at a time. A remux is bounded by disk rather than CPU, and
			// two concurrent ones on the same spindle are slower than two in
			// sequence while also filling the work directory twice as fast.
			MaxConcurrent: 1,
		})
		w.log.Info("remuxing is available", "ffmpeg", toolchain.FFmpeg.Version)
	}

	runtime, err := NewRuntime(Config{
		Owner: owner(),
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
