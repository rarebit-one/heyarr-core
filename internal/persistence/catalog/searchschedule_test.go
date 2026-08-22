package catalog_test

import (
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
)

// The search scheduler's storage half (#130).
//
// The cadence policy is table-tested in the domain and the beat is tested in
// the controller. What these add is the pair of things only a real database
// can be wrong about: which wants the due query returns, and whether the
// compare-and-set actually refuses.

func TestAWantWithNoRowIsDueImmediately(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}

	due, err := h.cat.DueSearches(ctx, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("%d wants are due, want 1 — a want nobody has ever looked for is the most "+
			"urgent kind there is", len(due))
	}
	if due[0].Schedule.Name != "missing" {
		t.Errorf("schedule = %q, want %q", due[0].Schedule.Name, "missing")
	}
	if !due[0].FirstEver {
		t.Error("a want with no row is not reported as never having been looked for")
	}
	if due[0].Fruitless != 0 {
		t.Errorf("fruitless = %d, want 0", due[0].Fruitless)
	}
}

// The hazard sortableTimestamp exists for. RFC3339Nano TRIMS trailing zeros,
// so "…:00Z" and "…:00.000000001Z" compare in the wrong order as TEXT — and
// the due query is exactly that comparison.
//
// A whole second and one nanosecond later is the smallest case that breaks it,
// and the spread on next_search_at means sub-second values are the norm rather
// than a curiosity.
func TestDueComparisonSurvivesASubSecondBoundary(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	// Next due exactly on the second.
	if ok, err := h.cat.RecordSearchScheduled(ctx, h.want,
		acquisition.MissingSearches(), 0, base.Add(-time.Hour), base); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("the first record did not apply")
	}

	// One nanosecond before: NOT due.
	due, err := h.cat.DueSearches(ctx, base.Add(-time.Nanosecond), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Errorf("%d wants due a nanosecond BEFORE the due time, want 0", len(due))
	}

	// One nanosecond after: due. Under RFC3339Nano this is the comparison that
	// silently answers "no", forever.
	due, err = h.cat.DueSearches(ctx, base.Add(time.Nanosecond), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("%d wants due a nanosecond AFTER the due time, want 1", len(due))
	}
	if due[0].Fruitless != 1 {
		t.Errorf("fruitless = %d on the second search of a want that has not moved, want 1",
			due[0].Fruitless)
	}
}

// The compare-and-set. Recording twice for the same pass advances the streak
// once, which is what stops two roles from doubling the interval twice.
func TestRecordingIsACompareAndSet(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s := acquisition.MissingSearches()

	first, err := h.cat.RecordSearchScheduled(ctx, h.want, s, 0, now, s.NextAt(now, 0, h.want))
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("the first record did not advance the schedule")
	}
	second, err := h.cat.RecordSearchScheduled(ctx, h.want, s, 0, now, s.NextAt(now, 0, h.want))
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("a second record for the same pass advanced the schedule again; two roles " +
			"would each count the same search")
	}
}

// A want that changes which question it is asking starts a fresh streak.
// Carrying a MISSING want's week of silence into its first upgrade search
// would start that search two weeks late.
func TestChangingScheduleResetsTheStreak(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if _, err := h.cat.RecordSearchScheduled(ctx, h.want,
		acquisition.MissingSearches(), 6, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The want is now satisfied, so it is on the upgrade schedule.
	h.exec(t, `UPDATE acquisition_state SET managed = 1, content = 'satisfied',
		placement = 'satisfied' WHERE desired_item_id = ?`, h.want)

	due, err := h.cat.DueSearches(ctx, now.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("%d wants due, want 1", len(due))
	}
	if due[0].Schedule.Name != "upgrade" {
		t.Fatalf("schedule = %q, want %q", due[0].Schedule.Name, "upgrade")
	}
	if due[0].Fruitless != 0 {
		t.Errorf("fruitless = %d after changing schedule, want 0 — yesterday's silence says "+
			"nothing about a question nobody asked yet", due[0].Fruitless)
	}
}

// Only providers that can SEARCH. A download client being down says nothing
// about whether searching is worth attempting.
func TestIndexerHealthIgnoresProvidersThatCannotSearch(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	h.exec(t, `INSERT INTO provider_health
		(name, capabilities, healthy, detail, version, checked_at, created_at, updated_at)
		VALUES ('transmission', '["download"]', 0, '', '', ?, ?, ?)`, stamp, stamp, stamp)
	h.exec(t, `INSERT INTO provider_health
		(name, capabilities, healthy, detail, version, checked_at, created_at, updated_at)
		VALUES ('prowlarr', '["indexer","metadata"]', 1, '', '', ?, ?, ?)`, stamp, stamp, stamp)

	health, err := h.cat.IndexerHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(health) != 1 {
		t.Fatalf("IndexerHealth returned %d providers, want only the one that can search", len(health))
	}
	if !health["prowlarr"].Healthy {
		t.Error("the healthy indexer is not reported as healthy")
	}
}
