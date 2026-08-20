package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/scanner"
)

// reconcileLibraries turns the `libraries:` block of the configuration into
// libraries and library_roots rows, and enqueues a scan for each root.
//
// It belongs to the controller because control-plane configuration is the
// controller's (§7): the worker that runs the scan learns which root to walk
// from the job, not from a config file it might disagree about. And it is a
// JOB rather than a direct call because that is the only way roles are allowed
// to talk (§4, ADR-0002) — the worker may not even be the same process.
//
// The scan is enqueued at every start, deduped on the root. That is deliberate:
// the interesting case is not the first start but the hundredth, where the
// library changed while Heyarr was not running and nothing else would notice.
// It costs a stat per file when nothing changed (M1-12).
func reconcileLibraries(ctx context.Context, db *sqlite.DB, cfg config.Config, log *slog.Logger) error {
	if len(cfg.Libraries) == 0 {
		return nil
	}

	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Logger: log})
	if err != nil {
		return fmt.Errorf("controller: opening the event log: %w", err)
	}
	cat, err := catalog.New(catalog.Options{
		DB:       db,
		Events:   eventLog,
		PeerName: cfg.Peer.Name,
		PeerSite: cfg.Peer.Site,
		Logger:   log,
	})
	if err != nil {
		return fmt.Errorf("controller: opening the catalog: %w", err)
	}

	specs := make([]catalog.LibrarySpec, 0, len(cfg.Libraries))
	for _, lib := range cfg.Libraries {
		specs = append(specs, catalog.LibrarySpec{
			Name:        lib.Name,
			ContentType: lib.ContentType,
			Roots:       lib.Roots,
		})
	}
	roots, err := cat.ReconcileLibraries(ctx, specs)
	if err != nil {
		return fmt.Errorf("controller: reconciling libraries: %w", err)
	}

	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		return fmt.Errorf("controller: opening the job queue: %w", err)
	}
	for _, root := range roots {
		job, err := queue.Enqueue(ctx, jobs.EnqueueOptions{
			Type:    scanner.JobType,
			Payload: scanner.Payload{RootID: root.ID},
			// A scan already queued or running is the same scan. Without this,
			// a restart loop would queue one per start and they would fight
			// over the same root (ADR-0008).
			DedupeKey: scanner.DedupeKey(root.ID),
		})
		if err != nil {
			return fmt.Errorf("controller: enqueueing a scan of %s: %w", root.Path, err)
		}
		log.Info("library root reconciled",
			"root_id", root.ID, "library_id", root.LibraryID, "path", root.Path,
			"created", root.Created, "scan_job", job.ID)
	}
	log.Info("libraries reconciled", "libraries", len(specs), "roots", len(roots))
	return nil
}
