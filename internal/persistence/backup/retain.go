package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// EventPruned records that a local backup generation was removed by retention
// (invariant 7). It is backup bookkeeping, so it does not move the generation —
// see meaningfulGeneration.
const EventPruned = "control.backup_pruned"

// pruningPrefix marks a generation that is being removed. Renaming it out of
// the generation namespace first makes the removal atomic as far as any reader
// is concerned; a crash part-way through RemoveAll leaves a directory the next
// Prune finishes, never a torn generation that looks like a backup.
const pruningPrefix = ".pruning-"

// Retention bounds the backups a node keeps of its OWN control plane (#697).
//
// The cadence writes a new generation every interval whenever anything moved —
// on a live node that is every interval, because background beats write — so
// without a bound the directory grows by the database's size every few
// minutes. Keeping only the newest is the wrong bound for the reason
// backupsync.DefaultRetain gives: when the newest is the copy written during
// the incident, there is nothing older to go back to. So retention is
// grandfather-father-son: the newest Last generations, plus the newest
// generation of each of the most recent Hourly hours and Daily days.
//
// A zero field uses the default; a negative one is refused at config load.
type Retention struct {
	Last   int
	Hourly int
	Daily  int
}

// DefaultRetention is an hour of generations at the default 5m cadence, then a
// day of hourly ones, then a week of daily ones.
var DefaultRetention = Retention{Last: 12, Hourly: 24, Daily: 7}

func (r Retention) withDefaults() Retention {
	if r.Last <= 0 {
		r.Last = DefaultRetention.Last
	}
	if r.Hourly <= 0 {
		r.Hourly = DefaultRetention.Hourly
	}
	if r.Daily <= 0 {
		r.Daily = DefaultRetention.Daily
	}
	return r
}

// PruneOptions configure a single retention pass.
type PruneOptions struct {
	// Dir is the directory Take writes generations into.
	Dir string
	// Events records each removal. Required (invariant 7).
	Events *events.Log
	// Retention is the policy. Zero fields use [DefaultRetention].
	Retention Retention
	// Protect names generations that must survive whatever the policy says —
	// the one the caller just took and is about to push, which is not
	// necessarily the newest if another Take raced it.
	Protect []int64
}

// Prune removes the local backup generations under opts.Dir that the
// retention policy does not keep, and returns the generations it removed.
//
// It only ever considers a directory it can identify: a generation-named
// directory whose manifest reads and validates. A staging directory from an
// in-flight Take, a torn generation, or anything an operator put there is left
// alone — deleting what it cannot name is not retention's call.
func Prune(ctx context.Context, opts PruneOptions) ([]int64, error) {
	if opts.Dir == "" {
		return nil, errors.New("backup: a backup directory is required to prune")
	}
	if opts.Events == nil {
		return nil, errors.New("backup: an event log is required — pruning a backup is a state transition (invariant 7)")
	}
	entries, err := os.ReadDir(opts.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("backup: listing backups to prune: %w", err)
	}

	var held []Manifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, pruningPrefix) {
			// A previous pass was interrupted after the rename; its event was
			// already recorded, so only the bytes are left to finish.
			if err := os.RemoveAll(filepath.Join(opts.Dir, name)); err != nil {
				return nil, fmt.Errorf("backup: finishing an interrupted prune: %w", err)
			}
			continue
		}
		gen, ok := parseGenerationName(name)
		if !ok {
			continue
		}
		art, present, err := loadIfPresent(filepath.Join(opts.Dir, name))
		if err != nil || !present || art.Manifest.Generation != gen {
			continue
		}
		held = append(held, art.Manifest)
	}

	keep := retained(held, opts.Retention.withDefaults())
	for _, g := range opts.Protect {
		keep[g] = true
	}

	var removed []int64
	for _, m := range held {
		if keep[m.Generation] {
			continue
		}
		name := generationName(m.Generation)
		doomed := filepath.Join(opts.Dir, pruningPrefix+name)
		if err := os.Rename(filepath.Join(opts.Dir, name), doomed); err != nil {
			return removed, fmt.Errorf("backup: pruning generation %d: %w", m.Generation, err)
		}
		if _, err := opts.Events.Emit(ctx, EventPruned, subjectBackup, name, prunedPayload{
			Generation: m.Generation,
			TakenAt:    m.TakenAt,
			SizeBytes:  m.SizeBytes,
		}); err != nil {
			return removed, err
		}
		if err := os.RemoveAll(doomed); err != nil {
			return removed, fmt.Errorf("backup: removing pruned generation %d: %w", m.Generation, err)
		}
		removed = append(removed, m.Generation)
	}
	return removed, nil
}

type prunedPayload struct {
	Generation int64     `json:"generation"`
	TakenAt    time.Time `json:"taken_at"`
	SizeBytes  int64     `json:"size_bytes"`
}

// retained is the set of generations the policy keeps. Generations order the
// backups (they are monotonic, where a wall clock may step); TakenAt only
// buckets them into hours and days, in UTC so a timezone change does not
// reshuffle what is kept.
func retained(held []Manifest, r Retention) map[int64]bool {
	sorted := append([]Manifest(nil), held...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Generation > sorted[j].Generation })

	keep := map[int64]bool{}
	for _, m := range sorted[:min(r.Last, len(sorted))] {
		keep[m.Generation] = true
	}
	keepNewestPerBucket(sorted, r.Hourly, keep, func(t time.Time) string {
		return t.UTC().Format("2006-01-02T15")
	})
	keepNewestPerBucket(sorted, r.Daily, keep, func(t time.Time) string {
		return t.UTC().Format("2006-01-02")
	})
	return keep
}

// keepNewestPerBucket walks newest first and keeps the first generation it
// meets in each bucket, until it has seen n distinct buckets.
func keepNewestPerBucket(newestFirst []Manifest, n int, keep map[int64]bool, bucket func(time.Time) string) {
	seen := map[string]bool{}
	for _, m := range newestFirst {
		b := bucket(m.TakenAt)
		if seen[b] {
			continue
		}
		if len(seen) == n {
			return
		}
		seen[b] = true
		keep[m.Generation] = true
	}
}

// parseGenerationName is generationName's inverse, and refuses anything
// generationName could not have produced.
func parseGenerationName(name string) (int64, bool) {
	if len(name) != len(generationName(0)) {
		return 0, false
	}
	g, err := strconv.ParseInt(name, 10, 64)
	if err != nil || g <= 0 || generationName(g) != name {
		return 0, false
	}
	return g, true
}
