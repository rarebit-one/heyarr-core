package scanner_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/scanner"
)

// treeSize is the acceptance criterion's number. It is large enough that a
// broken cache is unmistakable and small enough that the test stays a unit
// test: 5 000 files, scanned twice.
const treeSize = 5000

// TestRescanOfAnUnchangedTreeReadsNothing is the acceptance criterion, and the
// reason the whole package exists.
//
// It counts opens through an instrumented filesystem rather than timing the
// scan. Timing would pass with the cache removed on any machine with a warm
// page cache, and fail with the cache working on a loaded CI runner — it would
// measure the runner, not the code.
func TestRescanOfAnUnchangedTreeReadsNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.writeTree(treeSize)

	first := f.mustScan()
	if first.FilesEnqueued != treeSize {
		t.Fatalf("first scan enqueued %d files, want %d", first.FilesEnqueued, treeSize)
	}
	if first.FilesUnchanged != 0 {
		t.Fatalf("first scan found %d unchanged files in an empty cache", first.FilesUnchanged)
	}
	if got := f.drainIngests(); got != treeSize {
		t.Fatalf("first pass ingested %d files, want %d", got, treeSize)
	}

	opensFirst, statsFirst, lstatsFirst, readDirsFirst := f.fs.counts()
	// If the instrument never moves, every "zero opens" assertion below is
	// vacuous — a comment with a test's name on it. Pin that first.
	if opensFirst < treeSize {
		t.Fatalf("first pass opened %d files for %d candidates — the open counter is not "+
			"observing the reads it is supposed to prove absent", opensFirst, treeSize)
	}

	f.fs.reset()

	second := f.mustScan()
	ingestedSecond := f.drainIngests()

	// The headline criterion is asserted FIRST, deliberately. If a cheaper
	// assertion above it fails on the same sabotage, the expensive one is never
	// exercised and nobody ever finds out whether it works.
	opensSecond, statsSecond, _, _ := f.fs.counts()
	if opensSecond != 0 {
		t.Fatalf("the second scan opened %d files (%v); an unchanged rescan must read nothing — "+
			"this is the difference between a rescan taking seconds and taking hours",
			opensSecond, firstFew(f.fs.openedPaths(), 5))
	}
	if second.FilesEnqueued != 0 {
		t.Fatalf("second scan enqueued %d files; nothing changed", second.FilesEnqueued)
	}
	if second.FilesUnchanged != treeSize {
		t.Fatalf("second scan saw %d unchanged files, want %d", second.FilesUnchanged, treeSize)
	}
	if ingestedSecond != 0 {
		t.Fatalf("second pass ingested %d files; nothing changed", ingestedSecond)
	}
	// The second pass must still STAT everything: that is how it knows nothing
	// changed. A pass that stopped stat-ing would be a pass that stopped
	// noticing changes, and it would also report zero opens.
	if statsSecond+opensSecond == 0 {
		t.Fatal("the second scan touched the filesystem not at all — it did not look, rather than looking cheaply")
	}

	t.Logf("opens: first pass %d, second pass %d (stats %d then %d, lstats %d, readdirs %d)",
		opensFirst, opensSecond, statsFirst, statsSecond, lstatsFirst, readDirsFirst)
}

func firstFew(paths []string, n int) []string {
	if len(paths) <= n {
		return paths
	}
	return paths[:n]
}

// TestChangedFilesAreReIngestedAndNothingElseIs covers the three ways a file
// can change without its path changing. The inode case is the one a naive
// (size, mtime) cache gets wrong, and it is not exotic: rsync -t, a restore
// from backup and a torrent client rewriting a file all produce it.
func TestChangedFilesAreReIngestedAndNothingElseIs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		unixOnly     bool
		mutate       func(t *testing.T, full string, before os.FileInfo)
		wantReIngest bool
	}{
		{
			name: "mtime moves",
			mutate: func(t *testing.T, full string, before os.FileInfo) {
				later := before.ModTime().Add(time.Hour)
				if err := os.Chtimes(full, later, later); err != nil {
					t.Fatalf("touching %s: %v", full, err)
				}
			},
			wantReIngest: true,
		},
		{
			name: "size changes",
			mutate: func(t *testing.T, full string, _ os.FileInfo) {
				file, err := os.OpenFile(full, os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatalf("appending to %s: %v", full, err)
				}
				if _, err := file.WriteString("more bytes"); err != nil {
					t.Fatalf("appending to %s: %v", full, err)
				}
				if err := file.Close(); err != nil {
					t.Fatalf("closing %s: %v", full, err)
				}
			},
			wantReIngest: true,
		},
		{
			// Replaced by a different file of identical length whose mtime was
			// restored: only the inode says anything happened.
			name:     "replaced in place with the same size and mtime",
			unixOnly: true,
			mutate: func(t *testing.T, full string, before os.FileInfo) {
				replacement := full + ".new"
				same := make([]byte, before.Size())
				for i := range same {
					same[i] = 'x'
				}
				if err := os.WriteFile(replacement, same, 0o600); err != nil {
					t.Fatalf("writing %s: %v", replacement, err)
				}
				if err := os.Rename(replacement, full); err != nil {
					t.Fatalf("replacing %s: %v", full, err)
				}
				if err := os.Chtimes(full, before.ModTime(), before.ModTime()); err != nil {
					t.Fatalf("restoring the mtime of %s: %v", full, err)
				}
			},
			wantReIngest: true,
		},
		{
			name:   "nothing happens",
			mutate: func(*testing.T, string, os.FileInfo) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.unixOnly && runtime.GOOS == "windows" {
				t.Skip("device and inode numbers are not available on windows (fingerprint_windows.go)")
			}
			f := newFixture(t)
			const files = 20
			f.writeTree(files)
			if p := f.mustScan(); p.FilesEnqueued != files {
				t.Fatalf("first scan enqueued %d, want %d", p.FilesEnqueued, files)
			}
			f.drainIngests()
			f.fs.reset()

			target := filepath.Join(f.dir, "show-03", "season-00", "episode-0003.mkv")
			info, err := os.Stat(target)
			if err != nil {
				t.Fatalf("stat %s: %v", target, err)
			}
			tt.mutate(t, target, info)

			second := f.mustScan()
			want := int64(0)
			if tt.wantReIngest {
				want = 1
			}
			if second.FilesEnqueued != want {
				t.Fatalf("second scan enqueued %d files, want %d", second.FilesEnqueued, want)
			}
			if second.FilesUnchanged != int64(files)-want {
				t.Fatalf("second scan saw %d unchanged, want %d", second.FilesUnchanged, int64(files)-want)
			}

			pending := f.pendingIngests()
			if len(pending) != int(want) {
				t.Fatalf("%d ingest jobs pending, want %d", len(pending), want)
			}
			if want == 1 && pending[0].Path != target {
				t.Fatalf("re-ingested %s, want %s", pending[0].Path, target)
			}
			// Exactly one file was read, and it was that one.
			if got := f.fs.Opens(); got != int(want) {
				t.Fatalf("the second scan opened %d files (%v), want %d",
					got, f.fs.openedPaths(), want)
			}
		})
	}
}

// TestCancelledScanResumesWithoutRehashing is the property that makes a scan of
// a large library restartable. The cache is written as the scan goes, not at
// the end, so a worker draining on SIGTERM keeps what it established.
func TestCancelledScanResumesWithoutRehashing(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *fixtureOptions) {
		// A small batch so that the cancellation lands after several flushes
		// rather than before the first one.
		o.batchSize = 8
		o.progressInterval = 8
	})
	const files = 200
	f.writeTree(files)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Cancel at a deterministic point — after the scanner has probed the 50th
	// file it intends to enqueue — rather than after a sleep. Four tests in
	// this repo have failed on CI for sleeping instead of waiting for a
	// condition; this is the same mistake with a stopwatch instead of a poll.
	const cancelAfter = 50
	f.fs.onOpen = func(n int) {
		if n == cancelAfter {
			cancel()
		}
	}

	_, err := f.scan.Scan(ctx, scanner.Payload{RootID: f.rootID})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled scan returned %v, want context.Canceled", err)
	}
	if state, _ := f.scanRunState(); state != "cancelled" {
		t.Fatalf("the interrupted run recorded state %q, want cancelled — a deliberate stop is not a failure", state)
	}

	cachedAfterCancel := f.count(`SELECT count(*) FROM scanned_files WHERE root_id = ?`, f.rootID)
	if cachedAfterCancel == 0 {
		t.Fatal("the cancelled scan cached nothing — the cache is written at the end, so a resume is a restart")
	}
	if cachedAfterCancel >= files {
		t.Fatalf("the cancelled scan cached all %d files; it did not stop where it was told", cachedAfterCancel)
	}
	// Every file the interrupted scan handed to the queue must also be in the
	// cache. If the final flush were skipped because the context was cancelled,
	// the counts below would all still line up — they are relative to what was
	// cached — and the last batch would be silently re-read on the resume. This
	// is the assertion that notices.
	enqueuedBefore := len(f.pendingIngests())
	if cachedAfterCancel != enqueuedBefore {
		t.Fatalf("the cancelled scan enqueued %d files but cached %d fingerprints — "+
			"the difference is re-read on every resume", enqueuedBefore, cachedAfterCancel)
	}
	f.fs.onOpen = nil
	f.fs.reset()

	second := f.mustScan()
	if second.FilesUnchanged != int64(cachedAfterCancel) {
		t.Fatalf("the resumed scan re-examined %d of the %d files already cached",
			int64(cachedAfterCancel)-second.FilesUnchanged, cachedAfterCancel)
	}
	if second.FilesEnqueued != int64(files-cachedAfterCancel) {
		t.Fatalf("the resumed scan enqueued %d files, want the %d it had not reached",
			second.FilesEnqueued, files-cachedAfterCancel)
	}
	// Nothing that already landed was opened again.
	if got := f.fs.Opens(); got != files-cachedAfterCancel {
		t.Fatalf("the resumed scan opened %d files, want %d — it re-read work that had already landed",
			got, files-cachedAfterCancel)
	}
	// And across both passes every file is enqueued exactly once.
	total := f.count(`SELECT count(*) FROM jobs WHERE type = ?`, ingest.JobType)
	if total != files {
		t.Fatalf("%d ingest jobs exist across both passes (%d after the cancellation), want %d",
			total, enqueuedBefore, files)
	}
	t.Logf("cancelled after %d files cached; the resume enqueued %d and opened %d",
		cachedAfterCancel, second.FilesEnqueued, f.fs.Opens())
}

// TestAHostileFilesystemIsSkippedNotFatal. A library with one bad mount, one
// symlink loop and one dangling link must still scan: the alternative is that
// a single unreadable directory costs you the whole library.
func TestAHostileFilesystemIsSkippedNotFatal(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated privileges on windows")
	}
	f := newFixture(t)

	good := f.write("shows/good.mkv", "good bytes")
	f.write("shows/also-good.mp3", "more good bytes")

	// A symlink loop: a/self points back at a.
	loopDir := filepath.Join(f.dir, "a")
	if err := os.MkdirAll(loopDir, 0o750); err != nil {
		t.Fatalf("creating %s: %v", loopDir, err)
	}
	f.write("a/inside.mkv", "inside the loop")
	if err := os.Symlink("../a", filepath.Join(loopDir, "self")); err != nil {
		t.Fatalf("creating the symlink loop: %v", err)
	}

	// A dangling symlink.
	if err := os.Symlink(filepath.Join(f.dir, "nowhere.mkv"), filepath.Join(f.dir, "dangling.mkv")); err != nil {
		t.Fatalf("creating the dangling symlink: %v", err)
	}

	// A file nobody may read.
	locked := f.write("shows/locked.mkv", "cannot read this")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("locking %s: %v", locked, err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	lockedBites := os.Geteuid() != 0
	if !lockedBites {
		t.Log("running as root: mode 0000 does not bite, so the unreadable-file case is not exercised")
	}

	// An unreadable directory — the bad-mount case.
	badDir := filepath.Join(f.dir, "badmount")
	if err := os.MkdirAll(badDir, 0o750); err != nil {
		t.Fatalf("creating %s: %v", badDir, err)
	}
	f.write("badmount/hidden-away.mkv", "unreachable")
	if err := os.Chmod(badDir, 0o000); err != nil {
		t.Fatalf("locking %s: %v", badDir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(badDir, 0o750) })

	progress, err := f.scan.Scan(t.Context(), scanner.Payload{RootID: f.rootID})
	if err != nil {
		t.Fatalf("a hostile filesystem made the scan fatal: %v", err)
	}
	if state, _ := f.scanRunState(); state != "completed" {
		t.Fatalf("the run recorded state %q, want completed", state)
	}
	if progress.Errors == 0 {
		t.Fatal("the scan reported no errors over a dangling symlink, an unreadable directory and a loop")
	}

	enqueued := map[string]bool{}
	for _, p := range f.pendingIngests() {
		enqueued[p.RelPath] = true
	}
	for _, want := range []string{"shows/good.mkv", "shows/also-good.mp3", "a/inside.mkv"} {
		if !enqueued[want] {
			t.Errorf("the good file %s was not enqueued; got %v", want, keys(enqueued))
		}
	}
	if enqueued["dangling.mkv"] {
		t.Error("a dangling symlink was enqueued — ingest would fail on it five times")
	}
	if lockedBites && enqueued["shows/locked.mkv"] {
		t.Error("an unreadable file was enqueued rather than skipped")
	}
	if enqueued["badmount/hidden-away.mkv"] {
		t.Error("a file under an unreadable directory was enqueued")
	}
	if got := len(enqueued); got != 3 && !lockedBites {
		t.Logf("enqueued %d paths as root: %v", got, keys(enqueued))
	}
	_ = good
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAVanishedPathMarksTheAssetMissingAndKeepsTheBlob. ADR-0018: deletion is
// logical. A scanner that unlinked bytes because a mount was not ready when it
// ran would be the most destructive component in the system.
func TestAVanishedPathMarksTheAssetMissingAndKeepsTheBlob(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	// Two paths, identical bytes: one blob, two assets (§13). Proving that here
	// as well as in the acceptance script is cheap and it is the case a
	// hash-keyed asset table would get wrong.
	f.write("films/one.mkv", "identical bytes")
	f.write("films/two.mkv", "identical bytes")
	f.write("films/three.mkv", "different bytes entirely")

	if p := f.mustScan(); p.FilesEnqueued != 3 {
		t.Fatalf("first scan enqueued %d files, want 3", p.FilesEnqueued)
	}
	f.ingestPending()

	if got := f.count(`SELECT count(*) FROM blobs`); got != 2 {
		t.Fatalf("%d blobs for three files of which two are identical, want 2", got)
	}
	if got := f.count(`SELECT count(*) FROM assets`); got != 3 {
		t.Fatalf("%d assets for three files, want 3", got)
	}

	vanished := filepath.Join(f.dir, "films", "two.mkv")
	var blobOfVanished string
	if err := f.db.Reader().QueryRowContext(t.Context(),
		`SELECT blob_hash FROM assets WHERE source_path = ?`, vanished).Scan(&blobOfVanished); err != nil {
		t.Fatalf("reading the asset's blob: %v", err)
	}
	if err := os.Remove(vanished); err != nil {
		t.Fatalf("removing %s: %v", vanished, err)
	}

	sub := f.log.Subscribe(64, events.TypeAssetMissing)
	defer sub.Close()

	second := f.mustScan()
	if second.FilesMissing != 1 {
		t.Fatalf("the scan marked %d assets missing, want 1", second.FilesMissing)
	}

	// The asset is missing...
	var missingSince *string
	if err := f.db.Reader().QueryRowContext(t.Context(),
		`SELECT missing_since FROM assets WHERE source_path = ?`, vanished).Scan(&missingSince); err != nil {
		t.Fatalf("reading missing_since: %v", err)
	}
	if missingSince == nil {
		t.Fatal("the vanished path's asset was not marked missing")
	}
	// ...but it is still there, and so is its blob, and so are the bytes.
	if got := f.count(`SELECT count(*) FROM assets`); got != 3 {
		t.Fatalf("%d assets after one path vanished, want 3 — deletion is logical (ADR-0018)", got)
	}
	if got := f.count(`SELECT count(*) FROM blobs WHERE hash = ?`, blobOfVanished); got != 1 {
		t.Fatal("the vanished path's blob was deleted — bytes are reclaimed by GC, never by a scan (ADR-0018)")
	}
	if got := f.count(`SELECT count(*) FROM blobs`); got != 2 {
		t.Fatalf("%d blobs after one path vanished, want 2", got)
	}
	// The fingerprint is forgotten, so the path returning re-ingests it.
	if got := f.count(`SELECT count(*) FROM scanned_files WHERE root_id = ? AND path = ?`,
		f.rootID, "films/two.mkv"); got != 0 {
		t.Fatal("the vanished path is still in the fingerprint cache; if it came back it would look unchanged")
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != events.TypeAssetMissing {
			t.Fatalf("event type %s, want %s", ev.Type, events.TypeAssetMissing)
		}
		if !strings.Contains(string(ev.Payload), "blob_retained") {
			t.Errorf("content.asset.missing payload does not say the blob was retained: %s", ev.Payload)
		}
	default:
		t.Fatal("no content.asset.missing event was published")
	}

	// The file coming back clears the flag rather than creating a second asset.
	f.write("films/two.mkv", "identical bytes")
	if p := f.mustScan(); p.FilesEnqueued != 1 {
		t.Fatalf("the returned file enqueued %d ingests, want 1", p.FilesEnqueued)
	}
	f.ingestPending()
	if err := f.db.Reader().QueryRowContext(t.Context(),
		`SELECT missing_since FROM assets WHERE source_path = ?`, vanished).Scan(&missingSince); err != nil {
		t.Fatalf("reading missing_since: %v", err)
	}
	if missingSince != nil {
		t.Error("the returned file's asset is still marked missing")
	}
	if got := f.count(`SELECT count(*) FROM assets`); got != 3 {
		t.Fatalf("%d assets after the file returned, want 3", got)
	}
}

// TestADoubleScanEnqueuesOneJobPerFile pins the dedupe key. Without it, a scan
// running while the previous scan's ingests are still queued doubles the work
// for the entire library (ADR-0008).
func TestADoubleScanEnqueuesOneJobPerFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const files = 30
	f.writeTree(files)

	if p := f.mustScan(); p.FilesEnqueued != files {
		t.Fatalf("first scan enqueued %d, want %d", p.FilesEnqueued, files)
	}
	// Full ignores the cache, so the second scan tries to enqueue every file
	// again while the first scan's jobs are still pending. Only the dedupe key
	// stands between that and a doubled queue.
	second, err := f.scan.Scan(t.Context(), scanner.Payload{RootID: f.rootID, Full: true})
	if err != nil {
		t.Fatalf("full rescan: %v", err)
	}
	if second.FilesEnqueued != files {
		t.Fatalf("the full rescan enqueued %d files, want %d — Full must ignore the cache", second.FilesEnqueued, files)
	}

	total := f.count(`SELECT count(*) FROM jobs WHERE type = ?`, ingest.JobType)
	if total != files {
		t.Fatalf("%d ingest jobs exist after two scans of %d files — the dedupe key is not holding", total, files)
	}
}

// TestScanRunsRecordProgress. Progress that lives only in a log line is
// progress no API can serve (§76).
func TestScanRunsRecordProgress(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *fixtureOptions) { o.progressInterval = 4 })
	const files = 40
	f.writeTree(files)
	f.write("films/notes.txt.part", "a partial download")

	sub := f.log.Subscribe(256, events.TypeScanProgress)
	defer sub.Close()

	progress := f.mustScan()

	state, recorded := f.scanRunState()
	if state != "completed" {
		t.Fatalf("run state %q, want completed", state)
	}
	if recorded != progress {
		t.Fatalf("the scan_runs row says %+v, the scan returned %+v", recorded, progress)
	}
	if recorded.FilesSeen != files {
		t.Fatalf("files_seen %d, want %d", recorded.FilesSeen, files)
	}
	if recorded.FilesSkipped != 1 {
		t.Fatalf("files_skipped %d, want 1 (the .part file)", recorded.FilesSkipped)
	}
	if recorded.BytesSeen == 0 {
		t.Fatal("bytes_seen is zero")
	}

	sub.Close()
	var progressEvents int
	for range sub.Events() {
		progressEvents++
	}
	// One at the start, several during, one at the end.
	if progressEvents < 3 {
		t.Fatalf("%d system.scan.progress events for a %d-file scan reporting every 4 files, want several",
			progressEvents, files)
	}
	if got := f.count(`SELECT count(*) FROM events WHERE type = ?`, events.TypeScanProgress); got != progressEvents {
		t.Fatalf("%d progress events were published but %d are in the log — a subscriber saw something that was not recorded",
			progressEvents, got)
	}
}

// TestAnAbandonedRunDoesNotWedgeTheRoot. The one-live-run-per-root index stops
// two scans interleaving. A safety property that turns into a permanent outage
// after one SIGKILL is not a safety property.
func TestAnAbandonedRunDoesNotWedgeTheRoot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("films/one.mkv", "bytes")

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := f.db.Writer().ExecContext(t.Context(), `
		INSERT INTO scan_runs (id, root_id, state, started_at, updated_at)
		VALUES ('abandoned', ?, 'running', ?, ?)`, f.rootID, now, now); err != nil {
		t.Fatalf("planting an abandoned run: %v", err)
	}

	if p := f.mustScan(); p.FilesEnqueued != 1 {
		t.Fatalf("the scan enqueued %d files, want 1", p.FilesEnqueued)
	}
	var state string
	if err := f.db.Reader().QueryRowContext(t.Context(),
		`SELECT state FROM scan_runs WHERE id = 'abandoned'`).Scan(&state); err != nil {
		t.Fatalf("reading the abandoned run: %v", err)
	}
	if state != "cancelled" {
		t.Fatalf("the abandoned run is %q, want cancelled", state)
	}
}

// TestScanRefusesWhatItCannotDo.
func TestScanRefusesWhatItCannotDo(t *testing.T) {
	t.Parallel()

	t.Run("no root id", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		if _, err := f.scan.Scan(t.Context(), scanner.Payload{}); err == nil {
			t.Fatal("a scan with no root id succeeded")
		}
	})

	t.Run("unknown root", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		_, err := f.scan.Scan(t.Context(), scanner.Payload{RootID: "nope"})
		if !errors.Is(err, ingest.ErrRootNotFound) {
			t.Fatalf("error = %v, want ErrRootNotFound", err)
		}
	})

	t.Run("disabled root", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		if _, err := f.db.Writer().ExecContext(t.Context(),
			`UPDATE library_roots SET enabled = 0 WHERE id = ?`, f.rootID); err != nil {
			t.Fatalf("disabling the root: %v", err)
		}
		_, err := f.scan.Scan(t.Context(), scanner.Payload{RootID: f.rootID})
		if !errors.Is(err, ingest.ErrRootDisabled) {
			t.Fatalf("error = %v, want ErrRootDisabled", err)
		}
	})
}

// TestNewRefusesAnIncompleteScanner.
func TestNewRefusesAnIncompleteScanner(t *testing.T) {
	t.Parallel()
	if _, err := scanner.New(scanner.Options{}); err == nil {
		t.Fatal("a scanner with no store was constructed")
	}
	if _, err := scanner.New(scanner.Options{Store: nopStore{}}); err == nil {
		t.Fatal("a scanner with no queue was constructed")
	}
	if _, err := scanner.New(scanner.Options{Store: nopStore{}, Queue: nopQueue{}}); err != nil {
		t.Fatalf("a complete scanner was refused: %v", err)
	}
}

func TestFingerprintUnchanged(t *testing.T) {
	t.Parallel()
	base := scanner.Fingerprint{RelPath: "a.mkv", Size: 10, MtimeNS: 100, Dev: 1, Inode: 2}

	tests := []struct {
		name  string
		other scanner.Fingerprint
		want  bool
	}{
		{"identical", base, true},
		{"different size", scanner.Fingerprint{Size: 11, MtimeNS: 100, Dev: 1, Inode: 2}, false},
		{"different mtime", scanner.Fingerprint{Size: 10, MtimeNS: 101, Dev: 1, Inode: 2}, false},
		{"different inode", scanner.Fingerprint{Size: 10, MtimeNS: 100, Dev: 1, Inode: 3}, false},
		{"different device", scanner.Fingerprint{Size: 10, MtimeNS: 100, Dev: 2, Inode: 2}, false},
		// A zero pair means "this platform does not have them", not "device
		// zero". Treating it as a mismatch would make every Windows scan a full
		// re-ingest.
		{"cached row has no inode", scanner.Fingerprint{Size: 10, MtimeNS: 100}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := base.Unchanged(tt.other); got != tt.want {
				t.Fatalf("Unchanged(%+v) = %v, want %v", tt.other, got, tt.want)
			}
			if got := tt.other.Unchanged(base); got != tt.want {
				t.Fatalf("Unchanged is not symmetric for %+v", tt.other)
			}
		})
	}
}

func TestDedupeKeysAreDistinct(t *testing.T) {
	t.Parallel()
	if scanner.DedupeKey("root-a") == scanner.DedupeKey("root-b") {
		t.Fatal("two roots share a scan dedupe key — one would block the other")
	}
	if !strings.HasPrefix(scanner.DedupeKey("root-a"), "scan:") {
		t.Fatalf("the scan dedupe key %q does not name what it dedupes", scanner.DedupeKey("root-a"))
	}
	if scanner.DedupeKey("r") == ingest.DedupeKey("r", "a") {
		t.Fatal("a scan and an ingest share a dedupe key")
	}
}

// nopStore and nopQueue exist only so that New's validation can be tested
// without a database. They are never exercised.
type nopStore struct{ scanner.Store }

type nopQueue struct{ scanner.Queue }

// #54: a file whose ingest job died must come back.
//
// The cache records a fingerprint at ENQUEUE time, so a job that exhausted its
// attempts leaves a row matching the disk perfectly with no asset behind it.
// Before this, every later scan skipped that file without reading it and it
// stayed out of the library for ever — invisible to fsck too, which reconciles
// the catalog against the CAS and would find it in neither.
func TestAFileWhoseIngestDiedIsScannedAgain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const files = 5
	f.writeTree(files)

	if p := f.mustScan(); p.FilesEnqueued != files {
		t.Fatalf("first scan enqueued %d, want %d", p.FilesEnqueued, files)
	}

	// One job dies; the rest land.
	victim := f.pendingIngests()[0]
	f.killIngest(victim.RelPath)
	f.drainIngests()
	f.fs.reset()

	second := f.mustScan()
	if second.FilesEnqueued != 1 {
		t.Fatalf("the rescan enqueued %d files, want exactly the 1 whose ingest died — "+
			"a file that never landed is not 'unchanged', it is missing", second.FilesEnqueued)
	}
	if second.FilesUnchanged != files-1 {
		t.Fatalf("the rescan re-examined %d files, want %d — only the dead one should come back",
			int64(files)-second.FilesUnchanged, files-1)
	}
	if got := f.pendingIngests(); len(got) != 1 || got[0].RelPath != victim.RelPath {
		t.Fatalf("the rescan enqueued %v, want just %s", got, victim.RelPath)
	}
	// It was actually re-read, not merely re-counted.
	if f.fs.Opens() == 0 {
		t.Error("the rescan enqueued the file without opening it — the count moved but nothing happened")
	}
}

// ...and a job that is still going is NOT re-enqueued. A resumed scan must not
// pay an open() per file for work that is simply still in the queue.
func TestAFileWhoseIngestIsStillQueuedIsLeftAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const files = 5
	f.writeTree(files)

	if p := f.mustScan(); p.FilesEnqueued != files {
		t.Fatalf("first scan enqueued %d, want %d", p.FilesEnqueued, files)
	}
	// Deliberately do NOT drain: every job is still pending.
	f.fs.reset()

	second := f.mustScan()
	if second.FilesEnqueued != 0 {
		t.Fatalf("the rescan re-enqueued %d files whose jobs are still queued — "+
			"pending is not failed, and the queue will get to them", second.FilesEnqueued)
	}
	if got := f.fs.Opens(); got != 0 {
		t.Errorf("the rescan opened %d files that were merely still queued", got)
	}
}

// The REAL ingest pipeline is what marks a file as landed, and the scanner is
// what reads that mark. This drives both, because a test that marks the cache
// itself is testing its own fixture.
//
// Without the mark, a rescan re-enqueues everything that was already ingested —
// which is not data loss, but it is a full re-hash of the library on every
// scan, and the fingerprint cache exists precisely to avoid that.
func TestARealIngestMarksTheFileAsLandedSoARescanSkipsIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const files = 5
	f.writeTree(files)

	if p := f.mustScan(); p.FilesEnqueued != files {
		t.Fatalf("first scan enqueued %d, want %d", p.FilesEnqueued, files)
	}
	f.ingestPending() // the real pipeline, not a fixture shortcut
	f.fs.reset()

	second := f.mustScan()
	if second.FilesEnqueued != 0 {
		t.Fatalf("the rescan re-enqueued %d files after a real ingest — nothing marked them landed",
			second.FilesEnqueued)
	}
	if second.FilesUnchanged != files {
		t.Fatalf("the rescan saw %d unchanged, want %d", second.FilesUnchanged, files)
	}
	if got := f.fs.Opens(); got != 0 {
		t.Errorf("the rescan opened %d files that were already in the library", got)
	}
}
