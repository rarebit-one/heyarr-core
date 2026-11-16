package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// 🔴 The running backup beat prunes old local generations (#697).
//
// backup.Prune is proven in its own package; what this proves is that
// something CALLS it — the lesson downloadbeat_test.go records. Before #697 the
// beat took a generation every interval and nothing ever removed one, so a
// live node's backup directory grew by a whole database every few minutes.
//
// Two week-old generations are seeded before start. With retention at one of
// each tier, the beat's first cycle takes a real backup — a fresh controller
// has already recorded more events than either seeded generation, so the new
// one is the newest — and prunes both seeds.
func TestTheBackupBeatPrunesOldLocalGenerations(t *testing.T) {
	cfg := testConfig(t)
	cfg.Backup.Interval = "100ms"
	cfg.Backup.Retain = config.BackupRetain{Last: 1, Hourly: 1, Daily: 1}
	cfg.Backup.Dir = filepath.Join(cfg.DataDir, "backups")
	old := time.Now().UTC().Add(-7 * 24 * time.Hour)
	for g := int64(1); g <= 2; g++ {
		seedBackupGeneration(t, cfg.Backup.Dir, g, old.Add(time.Duration(g)*time.Minute))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(cfg, discard()).Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("the controller did not stop within 15s of cancellation")
		}
	})

	// A liveness bound, not a latency claim, for the reason waitForMigratedDatabase gives.
	deadline := time.Now().Add(30 * time.Second)
	for seededGenerationsHeld(t, cfg.Backup.Dir) > 0 {
		select {
		case err := <-done:
			t.Fatalf("the controller exited before the beat pruned: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 30s the seeded week-old generations are still held — the beat "+
				"takes backups and never prunes them, so the directory grows without bound (#697); "+
				"dir: %v", listDir(t, cfg.Backup.Dir))
		}
		time.Sleep(20 * time.Millisecond)
	}

	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var pruned int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM events WHERE type = ?`,
		backup.EventPruned).Scan(&pruned); err != nil {
		t.Fatal(err)
	}
	if pruned < 2 {
		t.Errorf("%d %s events for 2 pruned generations — a prune is a state transition "+
			"(invariant 7)", pruned, backup.EventPruned)
	}
}

func seedBackupGeneration(t *testing.T, dir string, g int64, takenAt time.Time) {
	t.Helper()
	gd := filepath.Join(dir, generationDirName(g))
	if err := os.MkdirAll(gd, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gd, backup.SnapshotFile), []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(backup.Manifest{Core: backup.Core{
		SourcePeerID: "peer-a", Generation: g, SchemaVersion: 1,
		TakenAt: takenAt, Digest: "blake3:00", SizeBytes: 8,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gd, backup.ManifestFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// generationDirName mirrors the backup package's generation directory name.
func generationDirName(g int64) string { return fmt.Sprintf("%020d", g) }

func seededGenerationsHeld(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	for g := int64(1); g <= 2; g++ {
		if _, err := os.Stat(filepath.Join(dir, generationDirName(g))); err == nil {
			n++
		}
	}
	return n
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
