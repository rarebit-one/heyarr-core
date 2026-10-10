// Package worker executes leased jobs.
package worker

// PersonalWorker is the compute role for the personal profile (ADR-0107).
//
// It is a restricted subset of the full Worker: it registers only the four job
// types that make sense without a media library — gc_blobs, reconcile_peer,
// replicate_blob and chunk_blob — and starts two beats that keep those jobs
// flowing:
//
//   - A GC beat (every six hours) that enqueues a gc_blobs job with apply=true.
//     GC is NOT run at startup, for the same reason startUpgradeScan is not:
//     a restart is not a reason to immediately reclaim bytes. The first
//     scheduled tick is enough.
//   - A convergence beat (every five minutes, at startup) that enqueues a
//     reconcile_peer job. On a personal node there are no libraries, so
//     §19's canonical-blob desired set is empty and pins alone drive the diff
//     (tracking issue #736).
//
// Everything the full Worker does that belongs to the media domain — ingest,
// scanning, acquisition, provider health, probing, remuxing, subtitle
// extraction — is absent. Those job types are never registered, so a job of
// that type stays PENDING AND VISIBLE rather than being claimed and failed,
// which is ADR-0025's guarantee for absent capabilities.
//
// The personal profile controller (mnemosyne serve) does not start a
// reconciliation beat or a GC beat, so the PersonalWorker is the only process
// that enqueues those jobs. A Mnemosyne deployment with no worker running will
// never run GC or converge its vault blobs to remote peers.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// gcBeatInterval is how often the personal worker enqueues a gc_blobs sweep.
//
// Six hours matches upgradeScanInterval: both are housekeeping tasks rather
// than correctness checks, and running GC every five minutes on a personal
// vault that changes slowly wastes I/O for no benefit.
const gcBeatInterval = 6 * time.Hour

// personalConvergenceInterval is how often the personal worker enqueues a
// reconcile_peer cycle.
//
// Five minutes matches the media controller's reconcileInterval. Vault blobs
// pinned to a remote peer should converge at the same cadence as library blobs;
// there is no reason to be slower when the cost is identical (one indexed scan
// over the pins table, nothing else).
const personalConvergenceInterval = 5 * time.Minute

// gcDedupeKey prevents two gc_blobs sweeps from queuing behind each other.
// The full sweep is the only GC variant on a personal node, so one key suffices.
const gcDedupeKey = "gc_blobs:sweep"

// PersonalWorker is the compute role for the personal profile (ADR-0107).
type PersonalWorker struct {
	cfg config.Config
	log *slog.Logger
}

// NewPersonalWorker constructs the personal worker.
func NewPersonalWorker(cfg config.Config, log *slog.Logger) *PersonalWorker {
	return &PersonalWorker{cfg: cfg, log: log.With("role", "worker")}
}

// Name identifies the role in logs and supervision.
func (pw *PersonalWorker) Name() string { return "worker" }

// Run claims and executes personal-profile jobs until ctx is cancelled, then
// drains. It follows the same schema-wait discipline as the full Worker: the
// schema is owned by the controller, so the worker waits for it rather than
// migrating independently.
func (pw *PersonalWorker) Run(ctx context.Context) error {
	pw.log.Info("mnemosyne worker started", "database", pw.cfg.Database.Path)

	// Startup uses a sibling context (not the shutdown context) for the same
	// reason the full Worker's startup does: a SIGTERM mid-startup should produce
	// a clean stop, not a startup error the next start has to redo.
	startupCtx, cancelStartup := context.WithTimeout(context.WithoutCancel(ctx), schemaWait+time.Minute)
	defer cancelStartup()

	db, err := sqlite.Open(startupCtx, sqlite.Options{Path: pw.cfg.Database.Path, Logger: pw.log})
	if err != nil {
		return fmt.Errorf("worker: opening database: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			pw.log.Error("closing database", "error", err)
		}
	}()

	// The worker does NOT migrate. The schema is owned by the controller (§7,
	// ADR-0003). A personal worker started before any controller has ever run
	// waits here, which is an ordinary startup state when the roles start
	// concurrently (ADR-0002).
	if err := waitForSchema(ctx, db, pw.log, schemaWait); err != nil {
		if ctx.Err() != nil {
			pw.log.Info("mnemosyne worker stopped while waiting for the schema")
			return nil
		}
		return err
	}

	store, err := cas.OpenFS(pw.cfg.CAS.Root)
	if err != nil {
		return fmt.Errorf("worker: opening the content store: %w", err)
	}

	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Logger: pw.log})
	if err != nil {
		return fmt.Errorf("worker: opening the event log: %w", err)
	}

	cat, err := catalog.New(catalog.Options{
		DB:         db,
		Events:     eventLog,
		PeerName:   pw.cfg.Peer.Name,
		PeerSite:   pw.cfg.Peer.Site,
		Logger:     pw.log,
		LocalStore: store,
	})
	if err != nil {
		return fmt.Errorf("worker: opening the catalog: %w", err)
	}

	// Resolve the self peer now, so a misconfigured peer name is a startup
	// failure rather than a job failure (ADR-0010).
	peerID, err := cat.SelfPeer(startupCtx)
	if err != nil {
		return fmt.Errorf("worker: resolving this peer: %w", err)
	}

	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		return fmt.Errorf("worker: opening the job queue: %w", err)
	}

	integrityOpts := integrity.Options{
		Store:   store,
		Catalog: cat,
		Logger:  pw.log,
		// ADR-0018's placement-pin durability guard. The personal worker runs GC
		// on a node whose vault blobs carry placement pins, so the guard is
		// essential: without it GC would reclaim bytes that a pin promises are
		// here (#723). lazyDurability resolves the peer key on first use, so the
		// worker and controller can start concurrently (ADR-0002, ADR-0010).
		Durability: newLazyDurability(pw.cfg.DataDir, peerID, db.Writer(), pw.log),
	}
	checker, err := integrity.NewChecker(integrityOpts)
	if err != nil {
		return fmt.Errorf("worker: building the integrity checker: %w", err)
	}
	collector, err := integrity.NewCollector(integrityOpts)
	if err != nil {
		return fmt.Errorf("worker: building the garbage collector: %w", err)
	}

	// members is the live membership closure used by the transfer puller to
	// authenticate peers. It is built here rather than stored in a struct so
	// that the closure captures only what it uses.
	members := func(ctx context.Context) (map[string]bool, error) {
		peers, err := cat.Peers(ctx)
		if err != nil {
			return nil, err
		}
		roster := make(map[string]bool, len(peers))
		for _, p := range peers {
			roster[p.PeerID] = true
		}
		return roster, nil
	}
	// lazyPuller defers private-key resolution until the first transfer, for the
	// same reason newLazyDurability does: the controller writes the key and the
	// roles start concurrently (ADR-0002, ADR-0010).
	puller := lazyPuller(pw.cfg.DataDir, peerID, store, cat, members, pw.log)

	registry := NewRegistry()

	// gc_blobs — one at a time; see GCHandler for why.
	registry.Register(integrity.GCJobType, Registration{
		Handler:       GCHandler(collector, pw.log),
		MaxConcurrent: 1,
	})
	// reconcile_peer — one cycle at a time; see ReconcilePeerRegistration for why.
	registry.Register(replication.ReconcilePeerJobType,
		ReconcilePeerRegistration(cat, queue, pw.log))
	// replicate_blob — bounded concurrency; see ReplicateBlobRegistration for why.
	registry.Register(replication.ReplicateBlobJobType, ReplicateBlobRegistration(TransferDeps{
		Catalog: cat,
		Store:   store,
		Puller:  puller,
		Logger:  pw.log,
	}))
	// chunk_blob — bounded concurrency; see ChunkBlobRegistration for why.
	registry.Register(manifests.ChunkBlobJobType, ChunkBlobRegistration(ChunkDeps{
		Store:     store,
		Manifests: cat,
		Index:     cat,
		Checker:   checker,
		Logger:    pw.log,
	}))

	workerID := personalOwner()
	runtime, err := NewRuntime(Config{Owner: workerID}, queue, registry, pw.log)
	if err != nil {
		return fmt.Errorf("worker: building the runtime: %w", err)
	}

	if ctx.Err() != nil {
		pw.log.Info("mnemosyne worker stopped during startup")
		return nil
	}

	// GC beat. Not run at startup: a restart is not a reason to reclaim bytes
	// immediately — the next scheduled tick is sufficient.
	startPersonalBeat(ctx, pw.log, "gc", gcBeatInterval, false, func(reason string) {
		if _, err := queue.Enqueue(ctx, jobs.EnqueueOptions{
			Type:      integrity.GCJobType,
			Payload:   integrity.GCPayload{Apply: true},
			DedupeKey: gcDedupeKey,
		}); err != nil {
			pw.log.Warn("could not enqueue gc_blobs sweep", "reason", reason, "error", err)
		}
	})

	// Peer convergence beat. Run at startup: a node that was down while a peer
	// lost or gained bytes should notice immediately on restart.
	startPersonalBeat(ctx, pw.log, "convergence", personalConvergenceInterval, true, func(reason string) {
		if _, err := queue.Enqueue(ctx, jobs.EnqueueOptions{
			Type:      replication.ReconcilePeerJobType,
			Payload:   replication.ReconcilePeerPayload{},
			DedupeKey: replication.ScopedReconcilePeerDedupeKey(""),
		}); err != nil {
			pw.log.Warn("could not enqueue peer convergence cycle", "reason", reason, "error", err)
		}
	})

	pw.log.Info("mnemosyne worker ready", "cas_root", store.Root(), "peer_id", peerID)
	if err := runtime.Run(ctx); err != nil {
		return err
	}
	pw.log.Info("mnemosyne worker stopped")
	return nil
}

// startPersonalBeat starts a periodic goroutine that calls fn on the given
// interval. If startup is true, fn is called once synchronously before the
// ticker begins. The goroutine stops when ctx is cancelled.
func startPersonalBeat(ctx context.Context, log *slog.Logger, name string, interval time.Duration, startup bool, fn func(string)) {
	if startup {
		fn("startup")
	}
	log.Info(name+" beat started", "interval", interval)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fn("beat")
			}
		}
	}()
}

// personalOwner identifies this worker instance in job leases. It follows the
// same uniqueness contract as owner() in worker.go.
func personalOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), uuid.Must(uuid.NewV7()).String()[:8])
}
