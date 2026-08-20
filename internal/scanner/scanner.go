// Package scanner walks library roots and enqueues ingest work for new or
// changed files (spec §9, M1-12).
//
// The whole design turns on one number. Hashing a 4 TB library takes hours;
// stat-ing it takes seconds. So the scanner never reads a file it has seen
// before unchanged, and "unchanged" is decided from (size, mtime_ns, dev,
// inode) held in scanned_files. Everything else here — the batched cache
// writes, the progress rows, the loop detection — exists to make that claim
// survive a real library: one that is being written to while it is scanned, has
// a bad mount in it, and whose scan is interrupted by a restart.
//
// The cache is written AS THE SCAN GOES rather than at the end. That is what
// makes a cancelled scan resumable: what already landed stays landed, and the
// job queue is durable (ADR-0008), so an ingest that was enqueued before the
// cancellation still runs.
package scanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
)

// JobType is the queue's name for this work. Nothing outside this package may
// spell it (ADR-0008).
const JobType = "scan_library"

// Payload is the scan_library job's payload.
type Payload struct {
	// RootID names the library root to walk.
	RootID string `json:"root_id"`
	// Full ignores the fingerprint cache and re-enqueues every candidate. It
	// exists for the case the cache cannot cover: a filesystem whose mtimes
	// were rewritten wholesale, typically by a restore. It is never the
	// default, because on a large library it means hours of hashing.
	Full bool `json:"full,omitempty"`
}

// DedupeKey is the queue's idempotency key for scanning one root. Two scans of
// the same root must not run at once: they would interleave their fingerprint
// writes and each compute a different, wrong set of vanished paths (ADR-0008).
func DedupeKey(rootID string) string { return "scan:" + rootID }

// Progress is what a scan has done so far. It is both the scan_runs row and the
// payload of system.scan.progress (§76).
type Progress struct {
	FilesSeen      int64 `json:"files_seen"`
	FilesEnqueued  int64 `json:"files_enqueued"`
	FilesUnchanged int64 `json:"files_unchanged"`
	FilesSkipped   int64 `json:"files_skipped"`
	FilesMissing   int64 `json:"files_missing"`
	Errors         int64 `json:"errors"`
	BytesSeen      int64 `json:"bytes_seen"`
}

// Run identifies one scan in flight.
type Run struct {
	ID        string
	RootID    string
	LibraryID string
	Progress  Progress
}

// Fingerprint is the cheap identity of a file on disk: everything that can be
// known about it without opening it.
type Fingerprint struct {
	// RelPath is slash-separated and relative to the library root. It is the
	// cache key, alongside the root.
	RelPath string
	Size    int64
	MtimeNS int64
	// Dev and Inode are zero where the platform does not expose them
	// (fingerprint_windows.go) and in rows written before it did.
	Dev   int64
	Inode int64
	// IngestFailed reports that this file was enqueued and its ingest job then
	// died, so the file is on disk and not in the library.
	//
	// The cache records a fingerprint at ENQUEUE time, which is right — the
	// queue is durable and the job outlives the scan. But it meant "we have
	// seen this path" and "this path is in the library" were the same fact, and
	// a job that exhausted its attempts left a row matching the disk perfectly
	// with no asset behind it. Every later scan skipped the file without
	// reading it, and it stayed out of the library for ever (#54).
	//
	// A row whose job is still pending or leased is NOT failed: that job is
	// going to run, and re-enqueueing would cost an open() per file on every
	// resumed scan for no gain.
	IngestFailed bool
}

// Unchanged reports whether a file on disk matches what the cache recorded.
//
// Size and mtime must both match. Device and inode must match too, but only
// when both sides have them: a zero pair means "not available" rather than
// "device zero", and treating an absent inode as a mismatch would make every
// scan on Windows a full re-ingest.
func (f Fingerprint) Unchanged(other Fingerprint) bool {
	if f.Size != other.Size || f.MtimeNS != other.MtimeNS {
		return false
	}
	if f.Inode != 0 && other.Inode != 0 && f.Inode != other.Inode {
		return false
	}
	if f.Dev != 0 && other.Dev != 0 && f.Dev != other.Dev {
		return false
	}
	return true
}

// Vanished is a cached path that is no longer on disk.
type Vanished struct {
	// RelPath is the cache key to forget.
	RelPath string
	// SourcePath is the absolute path the asset was recorded under.
	SourcePath string
}

// Store is the scanner's view of the control-plane database.
//
// The scanner decides what changed; this decides how that is written down. Two
// of these calls are deliberately coarse — RecordProgress and MarkVanished each
// do their writes and emit their events in ONE transaction — because a
// fingerprint cache updated in a different transaction from the run row that
// says how far the scan got is a cache that can disagree with itself after a
// crash (§76, ADR-0009).
type Store interface {
	// Root returns the library root's configuration.
	Root(ctx context.Context, rootID string) (ingest.Root, error)
	// Fingerprints returns every cached fingerprint for a root, keyed by
	// relative path.
	Fingerprints(ctx context.Context, rootID string) (map[string]Fingerprint, error)
	// BeginScan opens a scan_runs row, first cancelling any run left behind by
	// a process that died mid-scan.
	BeginScan(ctx context.Context, rootID string, now time.Time) (string, error)
	// RecordProgress persists the fingerprints observed since the last call,
	// updates the run's counters and emits system.scan.progress.
	RecordProgress(ctx context.Context, run Run, seen []Fingerprint, now time.Time) error
	// MarkVanished forgets cached paths that are no longer on disk, marks their
	// assets missing and emits content.asset.missing for each. It never
	// touches blobs: deletion is logical and the garbage collector owns bytes
	// (ADR-0018).
	MarkVanished(ctx context.Context, run Run, gone []Vanished, now time.Time) (int64, error)
	// FinishScan closes the run.
	FinishScan(ctx context.Context, run Run, state, failure string, now time.Time) error
}

// Queue is the subset of the job queue the scanner needs.
type Queue interface {
	Enqueue(ctx context.Context, opts jobs.EnqueueOptions) (jobs.Job, error)
}

// Clock is injected so a scan is testable without wall time (ADR-0017).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Defaults.
const (
	// DefaultBatchSize is how many fingerprints accumulate before they are
	// written. One transaction per file would make a 5 000-file scan 5 000
	// transactions; one transaction per scan would lose the whole cache when a
	// scan is cancelled, which is the case this feature exists to survive.
	DefaultBatchSize = 256
	// DefaultProgressInterval is how many files pass between progress events.
	DefaultProgressInterval = 1000
)

// Options configure a Scanner.
type Options struct {
	FS               FS
	Store            Store
	Queue            Queue
	Policy           Policy
	Clock            Clock
	Logger           *slog.Logger
	BatchSize        int
	ProgressInterval int
}

// Scanner walks library roots.
type Scanner struct {
	fs       FS
	store    Store
	queue    Queue
	policy   Policy
	clock    Clock
	log      *slog.Logger
	batch    int
	progress int
}

// New constructs a Scanner.
func New(opts Options) (*Scanner, error) {
	if opts.Store == nil {
		return nil, errors.New("scanner: a store is required")
	}
	if opts.Queue == nil {
		return nil, errors.New("scanner: a job queue is required")
	}
	fsys := opts.FS
	if fsys == nil {
		fsys = OSFS{}
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	policy := opts.Policy
	policy.Extensions = normaliseExtensions(policy.Extensions)
	if policy.MaxDepth <= 0 {
		policy.MaxDepth = DefaultMaxDepth
	}
	batch := opts.BatchSize
	if batch <= 0 {
		batch = DefaultBatchSize
	}
	progress := opts.ProgressInterval
	if progress <= 0 {
		progress = DefaultProgressInterval
	}
	return &Scanner{
		fs: fsys, store: opts.Store, queue: opts.Queue, policy: policy,
		clock: clock, log: logger, batch: batch, progress: progress,
	}, nil
}

// Scan walks one library root and enqueues ingest work for what changed.
//
// It returns the progress it reached even when it returns an error, because a
// scan that stopped halfway still did most of its work and the caller — a job
// handler that is about to log the outcome — has nothing else to say.
func (s *Scanner) Scan(ctx context.Context, p Payload) (Progress, error) {
	if p.RootID == "" {
		return Progress{}, errors.New("scanner: root_id must be set")
	}

	root, err := s.store.Root(ctx, p.RootID)
	if err != nil {
		return Progress{}, err
	}
	if !root.Enabled {
		return Progress{}, fmt.Errorf("%w: %s", ingest.ErrRootDisabled, root.ID)
	}
	if !filepath.IsAbs(root.Path) {
		return Progress{}, fmt.Errorf("scanner: library root %s has a relative path %q — "+
			"a root is an absolute path on a specific machine", root.ID, root.Path)
	}

	cached := map[string]Fingerprint{}
	if !p.Full {
		if cached, err = s.store.Fingerprints(ctx, p.RootID); err != nil {
			return Progress{}, err
		}
	}

	now := s.clock.Now()
	runID, err := s.store.BeginScan(ctx, p.RootID, now)
	if err != nil {
		return Progress{}, err
	}
	run := Run{ID: runID, RootID: root.ID, LibraryID: root.LibraryID}

	w := &walk{
		scanner: s,
		root:    root,
		full:    p.Full,
		cached:  cached,
		seen:    make(map[string]bool, len(cached)),
		run:     &run,
		visited: map[string]bool{},
	}

	s.log.Info("scan started",
		"run_id", runID, "root_id", root.ID, "path", root.Path,
		"cached_files", len(cached), "full", p.Full)

	walkErr := w.descend(ctx, root.Path, "", 0)

	// Flush whatever is still pending before deciding the outcome. A cancelled
	// scan must keep what it already established, or the next one re-hashes it.
	if err := w.flush(ctx); err != nil {
		walkErr = errors.Join(walkErr, err)
	}

	if walkErr != nil {
		state := "failed"
		if ctx.Err() != nil && errors.Is(walkErr, ctx.Err()) {
			// A worker draining on SIGTERM, or a lost lease. Deliberate, not an
			// incident — and the fingerprints written so far mean the next run
			// resumes rather than restarts.
			state = "cancelled"
		}
		s.finish(ctx, run, state, walkErr.Error())
		return run.Progress, walkErr
	}

	// Vanished paths are computed only from a COMPLETE walk. A scan that
	// stopped early has not visited most of the root, and treating unvisited as
	// vanished would mark a library missing because someone pressed ctrl-C.
	if gone := w.vanished(); len(gone) > 0 {
		missing, err := s.store.MarkVanished(ctx, run, gone, s.clock.Now())
		if err != nil {
			s.finish(ctx, run, "failed", err.Error())
			return run.Progress, err
		}
		run.Progress.FilesMissing = missing
	}

	s.finish(ctx, run, "completed", "")
	s.log.Info("scan completed",
		"run_id", runID, "root_id", root.ID,
		"files_seen", run.Progress.FilesSeen,
		"files_enqueued", run.Progress.FilesEnqueued,
		"files_unchanged", run.Progress.FilesUnchanged,
		"files_skipped", run.Progress.FilesSkipped,
		"files_missing", run.Progress.FilesMissing,
		"errors", run.Progress.Errors,
		"bytes_seen", run.Progress.BytesSeen)
	return run.Progress, nil
}

// finish closes the run. The context may already be cancelled — that is the
// common case for a cancelled scan — so the close runs on a detached context
// with a bound: a run row left saying "running" forever would block the next
// scan of that root on the one-live-run index.
func (s *Scanner) finish(ctx context.Context, run Run, state, failure string) {
	finishCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		finishCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
	}
	if err := s.store.FinishScan(finishCtx, run, state, failure, s.clock.Now()); err != nil {
		s.log.Error("could not close the scan run",
			"run_id", run.ID, "root_id", run.RootID, "state", state, "error", err)
	}
}

// walk carries the state of one traversal.
type walk struct {
	scanner *Scanner
	root    ingest.Root
	full    bool
	cached  map[string]Fingerprint
	seen    map[string]bool
	run     *Run
	pending []Fingerprint
	// visited keys directories already entered, so a symlink loop terminates.
	visited map[string]bool
	// sinceProgress counts files since the last progress event.
	sinceProgress int
}

// descend walks one directory. Errors below this point are counted and logged,
// never returned: a library with one bad mount must still scan. The only errors
// that stop a walk are cancellation and a failure to write the cache.
func (w *walk) descend(ctx context.Context, dir, relDir string, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > w.scanner.policy.MaxDepth {
		w.problem("directory nesting is deeper than the policy allows", dir, nil,
			"depth", depth, "max_depth", w.scanner.policy.MaxDepth)
		return nil
	}

	entries, err := w.scanner.fs.ReadDir(dir)
	if err != nil {
		// An unreadable directory is the bad-mount case: it is exactly one
		// subtree's worth of loss, and failing the scan would turn it into the
		// whole library's.
		w.problem("could not read directory", dir, err)
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		full := filepath.Join(dir, name)
		rel := name
		if relDir != "" {
			rel = relDir + "/" + name
		}

		isDir, isSymlink := entry.IsDir(), entry.Type()&os.ModeSymlink != 0
		if isSymlink {
			// Resolving is where a dangling symlink shows up, and it must not
			// be fatal — a library full of them is untidy, not broken.
			info, err := w.scanner.fs.Stat(full)
			if err != nil {
				w.problem("could not resolve symlink", full, err)
				continue
			}
			isDir = info.IsDir()
		}

		if isDir {
			if ok, reason := w.scanner.policy.AllowDir(name); !ok {
				w.scanner.log.Debug("skipping directory", "path", full, "reason", reason)
				continue
			}
			if w.enter(full) {
				if err := w.descend(ctx, full, rel, depth+1); err != nil {
					return err
				}
			}
			continue
		}
		if !entry.Type().IsRegular() && !isSymlink {
			// Sockets, devices, FIFOs. Opening one blocks forever, which is a
			// much worse failure than skipping it.
			w.scanner.log.Debug("skipping irregular file", "path", full, "mode", entry.Type().String())
			w.run.Progress.FilesSkipped++
			continue
		}
		if err := w.file(ctx, full, rel); err != nil {
			return err
		}
	}
	return nil
}

// enter reports whether a directory has not been walked already, recording it
// if it has not. It is what makes a symlink loop terminate.
//
// The key is (device, inode) where the platform has them, because that is what
// a loop actually is — two paths naming one directory — and a path-based check
// would be fooled by "a -> ." producing a new, longer, never-repeating path
// every time. Where inodes are unavailable it falls back to the cleaned path,
// and Policy.MaxDepth is the backstop.
func (w *walk) enter(dir string) bool {
	key := filepath.Clean(dir)
	if info, err := w.scanner.fs.Stat(dir); err == nil {
		if dev, inode := deviceAndInode(info); inode != 0 {
			key = fmt.Sprintf("%d:%d", dev, inode)
		}
	}
	if w.visited[key] {
		// Nothing above this is lost: the directory HAS been scanned, under
		// the first name it was reached by.
		w.scanner.log.Warn("skipping a directory already visited on this scan — symlink loop",
			"path", dir, "key", key)
		w.run.Progress.FilesSkipped++
		return false
	}
	w.visited[key] = true
	return true
}

// file decides one file's fate.
func (w *walk) file(ctx context.Context, full, rel string) error {
	info, err := w.scanner.fs.Lstat(full)
	if err != nil {
		// The file was there a moment ago when ReadDir listed it. A library
		// being written to while it is scanned does this constantly.
		w.problem("could not stat file", full, err)
		return nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if info, err = w.scanner.fs.Stat(full); err != nil {
			w.problem("could not resolve symlink", full, err)
			return nil
		}
	}

	mime, ok, reason := w.scanner.policy.AllowFile(rel)
	if !ok {
		w.scanner.log.Debug("skipping file", "path", full, "reason", reason)
		w.run.Progress.FilesSkipped++
		return nil
	}

	w.seen[rel] = true
	w.run.Progress.FilesSeen++
	w.run.Progress.BytesSeen += info.Size()

	dev, inode := deviceAndInode(info)
	fp := Fingerprint{
		RelPath: rel,
		Size:    info.Size(),
		MtimeNS: info.ModTime().UnixNano(),
		Dev:     dev,
		Inode:   inode,
	}

	// The line the whole package exists for. An unchanged file is not opened,
	// not hashed, and not enqueued — and its cache row is not rewritten either,
	// so a no-op rescan is a read-only pass over the database as well.
	//
	// "Unchanged" is not enough on its own: the file's ingest must not have
	// DIED. A fingerprint is written when a file is enqueued, so a job that
	// later exhausted its attempts leaves a row matching the disk perfectly and
	// no asset behind it — and skipping that is not recoverable, because
	// nothing else will ever look at the file again (#54).
	if !w.full {
		if previous, known := w.cached[rel]; known && !previous.IngestFailed && fp.Unchanged(previous) {
			w.run.Progress.FilesUnchanged++
			return w.maybeProgress(ctx)
		}
	}

	// Readability is probed only for a file that is about to be enqueued, which
	// is the same file ingest is about to read in full. So this costs one
	// open() on exactly the files that were going to be opened anyway, and an
	// unchanged file still sees no open at all.
	//
	// The alternative is enqueueing an ingest that fails five times on
	// permissions and then dies, per unreadable file, which turns one bad
	// directory into a queue full of noise.
	if err := w.readable(full); err != nil {
		w.problem("skipping unreadable file", full, err)
		return nil
	}

	if _, err := w.scanner.queue.Enqueue(ctx, jobs.EnqueueOptions{
		Type: ingest.JobType,
		Payload: ingest.Payload{
			RootID:  w.root.ID,
			Path:    full,
			RelPath: rel,
			MIME:    mime,
		},
		// Without this, a scan running while the previous one's ingests are
		// still queued enqueues every file twice (ADR-0008).
		DedupeKey: ingest.DedupeKey(w.root.ID, rel),
	}); err != nil {
		// Enqueueing is a database write. Failing it means the queue is
		// unavailable, which the next file will hit too — so this one stops
		// the scan rather than counting 5 000 identical errors.
		return fmt.Errorf("scanner: enqueueing ingest for %s: %w", full, err)
	}
	w.run.Progress.FilesEnqueued++

	// The fingerprint is recorded at ENQUEUE time, not at ingest time. The job
	// queue is durable (ADR-0008): once the job exists, the file is accounted
	// for whether or not this process survives to see it run. Recording it only
	// after a successful ingest would mean a scan interrupted between the two
	// re-walks and re-enqueues everything it had already handed off.
	w.pending = append(w.pending, fp)
	if len(w.pending) >= w.scanner.batch {
		if err := w.flush(ctx); err != nil {
			return err
		}
	}
	return w.maybeProgress(ctx)
}

// readable opens and immediately closes the file. It reads no bytes: the
// question is whether ingest will be allowed to, not what is inside.
func (w *walk) readable(full string) error {
	f, err := w.scanner.fs.Open(full)
	if err != nil {
		return err
	}
	return f.Close()
}

func (w *walk) maybeProgress(ctx context.Context) error {
	w.sinceProgress++
	if w.sinceProgress < w.scanner.progress {
		return nil
	}
	return w.flush(ctx)
}

// flush writes the pending fingerprints and the run's counters in one
// transaction, and emits system.scan.progress.
func (w *walk) flush(ctx context.Context) error {
	if len(w.pending) == 0 && w.sinceProgress == 0 {
		return nil
	}
	// Cancellation must not stop the flush: this is the write that makes the
	// cancelled scan resumable, and skipping it is how a resume becomes a
	// restart.
	flushCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		flushCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
	}
	run := *w.run
	if err := w.scanner.store.RecordProgress(flushCtx, run, w.pending, w.scanner.clock.Now()); err != nil {
		return fmt.Errorf("scanner: recording scan progress for %s: %w", w.root.ID, err)
	}
	w.pending = w.pending[:0]
	w.sinceProgress = 0
	return nil
}

// vanished lists cached paths the walk did not find.
func (w *walk) vanished() []Vanished {
	var gone []Vanished
	for rel := range w.cached {
		if w.seen[rel] {
			continue
		}
		gone = append(gone, Vanished{
			RelPath:    rel,
			SourcePath: filepath.Join(w.root.Path, filepath.FromSlash(rel)),
		})
	}
	sort.Slice(gone, func(i, j int) bool { return gone[i].RelPath < gone[j].RelPath })
	return gone
}

// problem counts and logs something that went wrong for one path. Nothing here
// is fatal, which is the point: the scanner's contract is that a library with
// one bad mount still scans.
func (w *walk) problem(msg, path string, err error, attrs ...any) {
	w.run.Progress.Errors++
	args := append([]any{"path", path}, attrs...)
	if err != nil {
		args = append(args, "error", err)
	}
	w.scanner.log.Warn(msg, args...)
}
