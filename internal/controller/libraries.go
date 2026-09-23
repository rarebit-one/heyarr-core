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
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// warnIfIngestWillCopy says so, once per root, when ingest from this library
// cannot use the hardlink rung and every adopted file will therefore be a full
// byte copy.
//
// ADR-0014 says cross-filesystem ingest "degrades to a copy with a warning,
// never an error". The degrading was implemented; the warning was not, and its
// absence is expensive in a way nothing else notices. Both cheap rungs of the
// ladder need the source and the destination in one MOUNT — reflink because
// cloning is a filesystem operation, hardlink because link(2) returns EXDEV
// across mounts whatever the device — so a CAS and a library that cannot link
// to each other means EVERY ingest is a full byte copy and adopting a library
// doubles its storage. That is the outcome ADR-0014 exists to avoid, and it
// arrives silently, one file at a time.
//
// It asks cas.HardlinkOutlook, which ATTEMPTS a hardlink rather than predicting
// one. Both previous instruments predicted: st_dev asked "one filesystem?" and
// was identical across the two bind mounts ProtectSystem=strict creates, so it
// was silent on the one host where the problem was real (#222); mount ids ask
// the comparison link(2) makes, which is right about mounts and still blind to
// a link refused for any other reason. The probe reads the errno of the
// operation the ingest will perform, so it cannot be blind to a cause nobody
// modelled — and it falls back to the mount inference only when the library
// holds no file to link, which is before any bytes are at stake.
//
// Once per root at startup, not once per file: this is a configuration
// question, answerable before any bytes move, and a million-line log is not a
// warning.
func warnIfIngestWillCopy(casRoot, libraryPath string, log *slog.Logger) {
	outlook, err := cas.HardlinkOutlook(libraryPath, casRoot)
	if err != nil {
		// Not fatal. The library may not be mounted yet, which the scan will
		// report far more usefully than a startup check can.
		log.Debug("could not establish whether ingest from the library can hardlink",
			"cas_root", casRoot, "path", libraryPath, "error", err)
		return
	}
	if !outlook.Known {
		log.Debug("whether ingest from the library can hardlink is not known",
			"cas_root", casRoot, "path", libraryPath,
			"instrument", outlook.Instrument, "evidence", outlook.Evidence)
		return
	}
	if outlook.CanHardlink {
		return
	}
	log.Warn("ingest from this library will COPY every file rather than share its bytes",
		"path", libraryPath,
		"cas_root", casRoot,
		// How it was established, not just what: a warning from the probe is a
		// fact, one from the inference is a prediction, and an operator
		// deciding whether to believe it needs to know which.
		"instrument", outlook.Instrument,
		"evidence", outlook.Evidence,
		"why", "the store and the library cannot hardlink to each other — most often "+
			"because they are in different mounts (they may even be on the same "+
			"filesystem), and neither reflink nor hardlink can cross a mount: under a "+
			"ProtectSystem=strict systemd unit a read-only library and a read-write "+
			"store are separate mounts",
		"cost", "adopting this library will consume a second full copy of it",
		"fix", "the store and the library must be in the SAME mount, not merely the same "+
			"filesystem — see #222 and reference-linux-host.md")
}

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

	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
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

		warnIfIngestWillCopy(cfg.CAS.Root, root.Path, log)
	}
	log.Info("libraries reconciled", "libraries", len(specs), "roots", len(roots))
	return nil
}
