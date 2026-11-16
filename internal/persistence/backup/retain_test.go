package backup_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
)

// Local backup retention (#697). Before it, nothing pruned <data_dir>/backups:
// the cadence wrote a whole-database copy every interval on any node whose
// background beats wrote anything, which is every live node.

// genName mirrors the package's generation directory name.
func genName(g int64) string { return fmt.Sprintf("%020d", g) }

// writeGeneration lays down a held generation directly, so a retention test
// can describe days of history without taking days of backups. Prune reads
// only the manifest, so the snapshot only needs to exist.
func writeGeneration(t *testing.T, dir string, g int64, takenAt time.Time) {
	t.Helper()
	gd := filepath.Join(dir, genName(g))
	if err := os.MkdirAll(gd, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gd, backup.SnapshotFile), []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeManifestJSON(t, filepath.Join(gd, backup.ManifestFile), backup.Manifest{Core: backup.Core{
		SourcePeerID: "peer-a", Generation: g, SchemaVersion: 1,
		TakenAt: takenAt, Digest: "blake3:00", SizeBytes: 8,
	}})
}

// heldGenerations lists the generation directories left under dir.
func heldGenerations(t *testing.T, dir string) []int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var held []int64
	for _, e := range entries {
		var g int64
		if _, err := fmt.Sscanf(e.Name(), "%020d", &g); err == nil && genName(g) == e.Name() {
			held = append(held, g)
		}
	}
	slices.Sort(held)
	return held
}

// 🔴 Retention keeps the newest few, then thins to hourly, then daily, and
// removes everything else.
func TestPruneKeepsGrandfatherFatherSon(t *testing.T) {
	t.Parallel()
	// One generation every 30 minutes from midnight UTC, for ten days.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(g int64) time.Time { return base.Add(time.Duration(g-1) * 30 * time.Minute) }
	const newest = 480 // 10 days * 48

	cases := []struct {
		name      string
		retention backup.Retention
		protect   []int64
		want      []int64
	}{
		{
			// Generation g is taken at minute 30(g-1), so the newest in hour h
			// is 2h+2 and the newest of day d is 48d+48.
			// Newest 3: 480 (hour 239:30), 479 (239:00), 478 (238:30).
			// Hourly 5: the newest in hours 239..235 — 480, 478, 476, 474, 472.
			// Daily 2: the newest of day 9 (480) and of day 8 (432).
			name:      "last, hourly and daily",
			retention: backup.Retention{Last: 3, Hourly: 5, Daily: 2},
			want:      []int64{432, 472, 474, 476, 478, 479, 480},
		},
		{
			// Every hourly and daily pick is already among the newest 60.
			name:      "a large last subsumes the coarser tiers",
			retention: backup.Retention{Last: 60, Hourly: 2, Daily: 1},
			want:      rangeOf(newest-59, newest),
		},
		{
			// A protected generation survives however old it is: the beat
			// protects the one it just took and is about to push.
			name:      "a protected generation is kept",
			retention: backup.Retention{Last: 1, Hourly: 1, Daily: 1},
			protect:   []int64{7},
			want:      []int64{7, 480},
		},
		{
			// Zero means the default (12, 24, 7), never "keep none": the
			// newest 12 (469..480), the newest of hours 239..216 (even
			// generations 434..480), and the newest of days 9..3 (480 down
			// to 192 in steps of 48).
			name:      "zero fields use the defaults",
			retention: backup.Retention{},
			want: slices.Concat([]int64{192, 240, 288, 336, 384, 432},
				evensOf(434, 468), rangeOf(469, 480)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			dir := filepath.Join(n.dir, "backups")
			for g := int64(1); g <= newest; g++ {
				writeGeneration(t, dir, g, at(g))
			}

			removed, err := backup.Prune(t.Context(), backup.PruneOptions{
				Dir: dir, Events: n.log, Retention: tc.retention, Protect: tc.protect,
			})
			if err != nil {
				t.Fatal(err)
			}

			got := heldGenerations(t, dir)
			if !slices.Equal(got, tc.want) {
				t.Errorf("held after prune = %v\nwant %v", got, tc.want)
			}
			if len(removed) != newest-len(tc.want) {
				t.Errorf("Prune reported %d removed, but %d generations are gone",
					len(removed), newest-len(got))
			}
		})
	}
}

func rangeOf(from, to int64) []int64 {
	var s []int64
	for g := from; g <= to; g++ {
		s = append(s, g)
	}
	return s
}

func evensOf(from, to int64) []int64 {
	var s []int64
	for g := from; g <= to; g += 2 {
		s = append(s, g)
	}
	return s
}

// 🔴 Prune touches nothing it cannot name.
//
// A staging directory belongs to a Take in flight; a generation-named
// directory with no valid manifest is a torn write Take will rebuild; anything
// else is the operator's. Only an interrupted prune's leftovers are finished.
func TestPruneLeavesWhatItCannotName(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	dir := filepath.Join(n.dir, "backups")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	writeGeneration(t, dir, 1, now.Add(-2*time.Minute))
	writeGeneration(t, dir, 2, now.Add(-time.Minute))
	writeGeneration(t, dir, 3, now)

	untouched := []string{".taking-123", genName(9), "operator-notes"}
	for _, name := range untouched {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	leftover := filepath.Join(dir, ".pruning-"+genName(5))
	if err := os.MkdirAll(leftover, 0o750); err != nil {
		t.Fatal(err)
	}

	if _, err := backup.Prune(t.Context(), backup.PruneOptions{
		Dir: dir, Events: n.log, Retention: backup.Retention{Last: 1, Hourly: 1, Daily: 1},
	}); err != nil {
		t.Fatal(err)
	}

	for _, name := range untouched {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed or became unreadable: %v", name, err)
		}
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("an interrupted prune's leftover was not finished: %v", err)
	}
	if got := heldGenerations(t, dir); !slices.Equal(got, []int64{3, 9}) {
		t.Errorf("held = %v, want the newest valid generation and the torn one", got)
	}
}

// 🔴 Pruning is a recorded transition that does not move the generation.
//
// One event per removed generation (invariant 7). And those events must not
// count as control-plane progress: if they did, every Take after a prune would
// see a new generation and write a fresh copy, so retention would make the
// cadence lose the idempotency it relies on.
func TestPruneRecordsEachRemovalWithoutAdvancingTheGeneration(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	var arts []backup.Artifact
	for i := range 3 {
		n.emit(t, fmt.Sprintf("change-%d", i))
		arts = append(arts, n.take(t, backup.TakeOptions{}))
	}
	dir := filepath.Join(n.dir, "backups")
	before, err := n.log.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	removed, err := backup.Prune(t.Context(), backup.PruneOptions{
		Dir: dir, Events: n.log, Retention: backup.Retention{Last: 1, Hourly: 1, Daily: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %v, want the two older generations", removed)
	}

	evs, err := n.log.Since(t.Context(), before, []string{backup.EventPruned}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Errorf("%d %s events for 2 removals, want one each", len(evs), backup.EventPruned)
	}

	again := n.take(t, backup.TakeOptions{})
	if again.Manifest.Generation != arts[2].Manifest.Generation {
		t.Errorf("the Take after a prune is generation %d, want %d — the prune's own "+
			"events were counted as control-plane progress", again.Manifest.Generation,
			arts[2].Manifest.Generation)
	}
	if got := heldGenerations(t, dir); len(got) != 1 {
		t.Errorf("held = %v, want only the newest", got)
	}
}
