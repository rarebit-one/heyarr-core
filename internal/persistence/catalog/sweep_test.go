package catalog_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// seedWants adds wants after the harness's own, with created_at values that tie
// in pairs, so a page boundary can fall between two wants of the same instant
// and the id tiebreak is exercised.
func seedWants(t *testing.T, h *harness, n int) []string {
	t.Helper()
	ids := []string{h.want}
	if _, err := h.cat.StartAcquisition(context.Background(), h.want); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("want-z%02d", i)
		created := fmt.Sprintf("2026-09-%02dT00:00:00Z", 1+i/2)
		// A work each: one want per target and profile.
		h.exec(t, `INSERT INTO works
			(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
			VALUES (?, 'movie', ?, ?, ?, 2016, '{}', ?, ?)`, "w-"+id, "movie:"+id+":2016", id, id, created, created)
		h.exec(t, `INSERT INTO desired_items
			(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
			VALUES (?, 'work', ?, NULL, 'q1', 1, '', ?, ?)`, id, "w-"+id, created, created)
		if _, err := h.cat.StartAcquisition(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// The reconciliation sweep pages through the WHOLE library (#942): every want
// exactly once, oldest first, however the page size divides it — not the same
// first page on every pass.
func TestDesiredItemsToReconcilePagesThroughEveryWant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	want := seedWants(t, h, 6) // 7 wants, pages of 3: 3 + 3 + 1

	var (
		got    []string
		cursor catalog.SweepCursor
		pages  int
	)
	for {
		ids, next, err := h.cat.DesiredItemsToReconcile(context.Background(), cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		got = append(got, ids...)
		if len(ids) < 3 {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("the sweep never reached a short page; the cursor is not advancing")
		}
	}
	if len(got) != len(want) {
		t.Fatalf("swept %d wants (%v), want all %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: %s, want %s (oldest first, id breaking ties)", i, got[i], want[i])
		}
	}
}

// The upgrade scan pages the same way, so a monitored want past the first page
// is still considered.
func TestScanForUpgradesPagesThroughEveryMonitoredWant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedWants(t, h, 6)
	h.exec(t, `UPDATE desired_items SET monitor = 0 WHERE id = 'want-z03'`) // 6 monitored

	var (
		considered int
		cursor     catalog.SweepCursor
	)
	for pages := 0; ; pages++ {
		page, err := h.cat.ScanForUpgrades(context.Background(), cursor, 4)
		if err != nil {
			t.Fatal(err)
		}
		considered += page.Considered
		if page.Considered < 4 {
			break
		}
		cursor = page.Next
		if pages > 10 {
			t.Fatal("the scan never reached a short page; the cursor is not advancing")
		}
	}
	if considered != 6 {
		t.Errorf("the scan considered %d monitored wants across its pages, want 6", considered)
	}
}
