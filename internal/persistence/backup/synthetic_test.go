package backup_test

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// This file holds ADVERSARIAL synthetic tests for the control-plane backup
// package: each probes a scenario the existing suite does not cover. It reuses
// the shared helpers from backup_test.go (newNode, node.emit, node.take,
// markerCount, fixedClock) — all in package backup_test.

// TestConcurrentTakeOnOneDB fires 8 goroutines at backup.Take against ONE live
// *sqlite.DB, sharing the same event log and destination directory. It probes
// VACUUM INTO + the temp-dir rename under concurrency: no panic, no data race
// (relies on -race), every returned Artifact opens, and — because no meaningful
// state moved between the takes — every take reports the SAME generation and
// resolves to the SAME directory (the concurrent-rename fallback in Take).
func TestConcurrentTakeOnOneDB(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "seed") // generation must be > 0 before a backup is allowed

	const workers = 8
	dir := filepath.Join(n.dir, "concurrent-backups")

	var (
		arts = make([]backup.Artifact, workers)
		errs = make([]error, workers)
		wg   sync.WaitGroup
		gate = make(chan struct{}) // release all goroutines together
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			arts[i], errs[i] = backup.Take(context.Background(), backup.TakeOptions{
				DB:           n.db,
				Events:       n.log,
				SourcePeerID: "peer-a",
				Dir:          dir,
				Clock:        fixedClock{t: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)},
			})
		}(i)
	}
	close(gate)
	wg.Wait()

	// No take may fail, and every one must be internally consistent.
	var wantGen int64 = -1
	var wantDir string
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent take %d failed: %v", i, errs[i])
		}
		if wantGen < 0 {
			wantGen = arts[i].Manifest.Generation
			wantDir = arts[i].Dir
		}
		if arts[i].Manifest.Generation != wantGen {
			t.Errorf("take %d reported generation %d, others report %d — concurrent takes of one unchanging state disagree",
				i, arts[i].Manifest.Generation, wantGen)
		}
		if arts[i].Dir != wantDir {
			t.Errorf("take %d landed in %s, others in %s — the same generation produced two directories",
				i, arts[i].Dir, wantDir)
		}
		// Every returned artefact must open (unsigned → digest-only verification).
		opened, err := backup.Open(context.Background(), arts[i].Dir, backup.OpenOptions{})
		if err != nil {
			t.Errorf("artefact from take %d does not open: %v", i, err)
			continue
		}
		_ = opened.Close()
	}
	if wantGen <= 0 {
		t.Fatalf("expected a positive generation, got %d", wantGen)
	}
}

// TestLargeDBSurvivesRoundTrip seeds ~2000 events, takes a backup, restores it
// to a fresh path, and asserts all 2000 survive and the restored snapshot opens
// clean (Open runs integrity_check + foreign_key_check internally, so a
// successful Open of the backup dir IS the integrity assertion).
func TestLargeDBSurvivesRoundTrip(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	const total = 2000
	for i := 0; i < total; i++ {
		n.emit(t, "bulk")
	}
	art := n.take(t, backup.TakeOptions{})

	// The backup directory itself must pass integrity_check (via Open).
	opened, err := backup.Open(t.Context(), art.Dir, backup.OpenOptions{})
	if err != nil {
		t.Fatalf("a 2000-event backup did not open cleanly (integrity_check failed): %v", err)
	}
	_ = opened.Close()

	dest := filepath.Join(t.TempDir(), "restored-large.db")
	if _, err := backup.Restore(t.Context(), art.Dir, dest, backup.RestoreOptions{}); err != nil {
		t.Fatalf("restore of a large backup: %v", err)
	}
	if got := markerCount(t, dest, "bulk"); got != total {
		t.Errorf("large-DB round trip lost rows: %d of %d 'bulk' events survived", got, total)
	}
	// Independently confirm the restored file passes integrity_check.
	assertIntegrityOK(t, dest)
}

// TestTruncatedSnapshotRefused takes a SIGNED backup, truncates snapshot.db to
// half its on-disk size, then opens with the signer's pubkey. The size field in
// the manifest exists precisely so a truncated transfer is caught before the
// digest is even computed — Open must REFUSE, never open a half file.
func TestTruncatedSnapshotRefused(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	n := newNode(t)
	n.emit(t, "seed")
	art := n.take(t, backup.TakeOptions{Signer: priv})

	snap := art.SnapshotPath()
	info, err := os.Stat(snap)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	if info.Size() < 2 {
		t.Fatalf("snapshot implausibly small (%d bytes) — cannot halve", info.Size())
	}
	if err := os.Truncate(snap, info.Size()/2); err != nil {
		t.Fatalf("truncate snapshot: %v", err)
	}

	opened, err := backup.Open(t.Context(), art.Dir, backup.OpenOptions{PublicKey: pub})
	if err == nil {
		_ = opened.Close()
		t.Fatal("a truncated snapshot was accepted by Open — size/digest guard did not fire")
	}
}

// TestZeroByteSnapshotRefused replaces snapshot.db with an empty file. Open must
// refuse: an empty file is neither the right size nor the right digest, and it is
// not a database.
func TestZeroByteSnapshotRefused(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "seed")
	art := n.take(t, backup.TakeOptions{})

	if err := os.WriteFile(art.SnapshotPath(), []byte{}, 0o600); err != nil {
		t.Fatalf("zero the snapshot: %v", err)
	}
	opened, err := backup.Open(t.Context(), art.Dir, backup.OpenOptions{})
	if err == nil {
		_ = opened.Close()
		t.Fatal("a zero-byte snapshot was accepted by Open")
	}
}

// TestRestoreTwiceIsIdempotent restores the same backup to the same destination
// path twice. The second must succeed (the copy is temp-then-rename, so it
// overwrites atomically) and the result must still be a valid, openable database
// with intact data — no corruption from restoring over an existing file.
func TestRestoreTwiceIsIdempotent(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	const total = 15
	for i := 0; i < total; i++ {
		n.emit(t, "keep")
	}
	art := n.take(t, backup.TakeOptions{})

	dest := filepath.Join(t.TempDir(), "restored-twice.db")
	if _, err := backup.Restore(t.Context(), art.Dir, dest, backup.RestoreOptions{}); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	if _, err := backup.Restore(t.Context(), art.Dir, dest, backup.RestoreOptions{}); err != nil {
		t.Fatalf("second restore over the same path: %v", err)
	}

	assertIntegrityOK(t, dest)
	if got := markerCount(t, dest, "keep"); got != total {
		t.Errorf("restore-twice corrupted data: %d of %d 'keep' events survived", got, total)
	}
}

// TestNegativeAgeReturnedAsMeasured builds a manifest whose TakenAt is in the
// future and asserts Age(now) is negative and returned as-is, not clamped to
// zero — an operator who sees "-3m" (a source clock ahead of ours) has learned
// something true.
func TestNegativeAgeReturnedAsMeasured(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	future := now.Add(3 * time.Minute)
	m := backup.Manifest{Core: backup.Core{TakenAt: future}}

	age := m.Age(now)
	if age >= 0 {
		t.Errorf("Age of a future-dated backup was %v, want a negative duration (not clamped)", age)
	}
	if age != -3*time.Minute {
		t.Errorf("Age = %v, want exactly -3m (returned as measured)", age)
	}
}

// TestMalformedManifestJSON writes garbage into manifest.json and asserts both
// ReadManifest and Open return an error (never panic) rather than acting on a
// half-parsed manifest.
func TestMalformedManifestJSON(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.emit(t, "seed")
	art := n.take(t, backup.TakeOptions{})

	garbage := []byte("{ this is not valid json at all, ]]] \x00\xff")
	if err := os.WriteFile(filepath.Join(art.Dir, backup.ManifestFile), garbage, 0o600); err != nil {
		t.Fatalf("write garbage manifest: %v", err)
	}

	if _, err := backup.ReadManifest(art.Dir); err == nil {
		t.Error("ReadManifest accepted a garbage manifest")
	}
	opened, err := backup.Open(t.Context(), art.Dir, backup.OpenOptions{})
	if err == nil {
		_ = opened.Close()
		t.Error("Open accepted a garbage manifest")
	}
}

// assertIntegrityOK opens a restored database file directly and runs
// integrity_check, asserting "ok".
func assertIntegrityOK(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open %s for integrity check: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	var result string
	if err := db.Reader().QueryRowContext(t.Context(), "PRAGMA integrity_check").Scan(&result); err != nil {
		t.Fatalf("integrity_check on %s: %v", dbPath, err)
	}
	if result != "ok" {
		t.Errorf("integrity_check on %s reported %q, want ok", dbPath, result)
	}
}
