package catalog_test

import (
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/desired"
	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// Followed sources, items and the shared want-creation path, against a real
// database (§55, M12). The domain is table-tested and the beat is tested in the
// controller; what these add is the pair of things only a real database can be
// wrong about — which sources the due query returns, whether the compare-and-set
// refuses, and whether the projection is genuinely idempotent.

func source(workID string, backfill followed.Backfill) followed.Source {
	return followed.Source{
		WorkID:           workID,
		Type:             followed.TypeTVSeries,
		FeedRef:          "tvdb:99",
		QualityProfileID: "q1",
		Monitor:          true,
		Backfill:         backfill,
		Reason:           "Kate watches this",
	}
}

func TestCreateFollowSourceIsDueImmediatelyAndEmits(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	before := h.eventCount(t)

	src, err := h.cat.CreateFollowSource(ctx, source("w1", followed.BackfillFromNow))
	if err != nil {
		t.Fatal(err)
	}
	if src.ID == "" {
		t.Fatal("a created source has no id")
	}
	if h.eventCount(t) != before+1 {
		t.Error("creating a followed source is a transition and must emit (invariant 7)")
	}

	due, err := h.cat.DueSources(ctx, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("%d sources are due, want 1 — a source nobody has polled is the most urgent", len(due))
	}
	if !due[0].FirstEver {
		t.Error("a never-polled source is not reported as first-ever")
	}
	if due[0].Schedule.Name != followed.FeedPoll().Name {
		t.Errorf("schedule = %q, want %q", due[0].Schedule.Name, followed.FeedPoll().Name)
	}
}

func TestFollowingTheSameFeedTwiceIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	if _, err := h.cat.CreateFollowSource(ctx, source("w1", followed.BackfillFromNow)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.CreateFollowSource(ctx, source("w1", followed.BackfillFromNow)); err == nil {
		t.Fatal("following the same series through the same feed twice must be refused")
	}
}

func TestRecordPollScheduledIsCompareAndSet(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	src, err := h.cat.CreateFollowSource(ctx, source("w1", followed.BackfillFromNow))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	sched := followed.FeedPoll()

	ok, err := h.cat.RecordPollScheduled(ctx, src.ID, sched, 0, base, sched.NextAt(base, 0, src.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the first advance did not apply")
	}
	// A second pass at the SAME now finds the row no longer due and loses the CAS.
	ok, err = h.cat.RecordPollScheduled(ctx, src.ID, sched, 0, base, sched.NextAt(base, 0, src.ID))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a second advance at the same instant must lose the compare-and-set")
	}
}

func TestRecordPollOutcomeResetsOrBacksOff(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	src, err := h.cat.CreateFollowSource(ctx, source("w1", followed.BackfillFromNow))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	// Two fruitless polls back the cadence off; the source is not due again soon.
	for i := 0; i < 2; i++ {
		if err := h.cat.RecordPollOutcome(ctx, src.ID, false, base); err != nil {
			t.Fatal(err)
		}
	}
	got, err := h.cat.FollowSource(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fruitless != 2 {
		t.Errorf("fruitless = %d after two empty polls, want 2", got.Fruitless)
	}
	backedOff := got.NextPollAt

	// A poll that finds something new resets the streak and pulls the next poll
	// back to the floor.
	if err := h.cat.RecordPollOutcome(ctx, src.ID, true, base); err != nil {
		t.Fatal(err)
	}
	got, err = h.cat.FollowSource(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fruitless != 0 {
		t.Errorf("fruitless = %d after a fruitful poll, want 0", got.Fruitless)
	}
	if !got.NextPollAt.Before(backedOff) {
		t.Error("a fruitful poll should pull the next poll earlier than the backed-off one")
	}
}

func TestUpsertItemIsIdempotentAndEmitsOncePerItem(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	fi := followed.FeedItem{Key: "S02E01", Title: "The Return", Attributes: map[string]string{"season": "2"}}

	before := h.eventCount(t)
	it, created, err := h.cat.UpsertItem(ctx, "w1", fi)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("a first sighting must report created")
	}
	if h.eventCount(t) != before+1 {
		t.Error("a discovered item is a transition and must emit once")
	}

	// A re-sight refreshes the mutable facts but is not a new item and emits nothing.
	fi.Title = "The Return (corrected)"
	after := h.eventCount(t)
	it2, created2, err := h.cat.UpsertItem(ctx, "w1", fi)
	if err != nil {
		t.Fatal(err)
	}
	if created2 {
		t.Error("re-sighting the same key must not create a second item")
	}
	if it2.ID != it.ID {
		t.Errorf("the re-sight got a new id %s, want %s", it2.ID, it.ID)
	}
	if h.eventCount(t) != after {
		t.Error("a re-sight is not a discovery and must not emit")
	}
	items, err := h.cat.ItemsForWork(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "The Return (corrected)" {
		t.Errorf("the item was not deduped-and-refreshed: %+v", items)
	}
}

func TestCreateDesiredItemProjectsAtItemScopeAndRefusesDuplicates(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	it, _, err := h.cat.UpsertItem(ctx, "w1",
		followed.FeedItem{Key: "S02E01", Title: "The Return"})
	if err != nil {
		t.Fatal(err)
	}
	want := followed.Source{
		WorkID: "w1", Type: followed.TypeTVSeries, FeedRef: "x",
		QualityProfileID: "q1", Monitor: true, Reason: "r",
	}.ProjectWant(it.ID)

	rec, err := h.cat.CreateDesiredItem(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Item.Scope != desired.ScopeItem || rec.Item.ItemID != it.ID {
		t.Errorf("projected want is %+v, want item-scoped at %s", rec.Item, it.ID)
	}
	// A resting acquisition row rides with it, or the reconciliation sweep could
	// never advance it.
	if _, err := h.cat.Acquisition(ctx, rec.Item.ID); err != nil {
		t.Errorf("the projected want has no acquisition state: %v", err)
	}

	// The same (item, profile) again is one want written twice — refused, and
	// classified so the worker can treat it as already-projected.
	_, err = h.cat.CreateDesiredItem(ctx, want)
	if err == nil {
		t.Fatal("projecting the same want twice must be refused")
	}
	if !catalog.IsDuplicateWant(err) {
		t.Errorf("the refusal is not classified as a duplicate want: %v", err)
	}
}

func TestMetadataHealthReportsOnlyMetadataProviders(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	h.exec(t, `INSERT INTO provider_health
		(name, capabilities, healthy, detail, version, checked_at, created_at, updated_at)
		VALUES ('tvdb', '["metadata"]', 0, 'down', '', ?, ?, ?)`, stamp, stamp, stamp)
	h.exec(t, `INSERT INTO provider_health
		(name, capabilities, healthy, detail, version, checked_at, created_at, updated_at)
		VALUES ('prowlarr', '["indexer"]', 1, 'ok', '', ?, ?, ?)`, stamp, stamp, stamp)

	health, err := h.cat.MetadataHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(health) != 1 {
		t.Fatalf("MetadataHealth reported %d providers, want only the metadata one", len(health))
	}
	if _, ok := health["tvdb"]; !ok {
		t.Error("the metadata provider is missing from MetadataHealth")
	}
}
