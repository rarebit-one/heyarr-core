package worker

import (
	"log/slog"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
)

// The sweep reaches a want past the first batch (#942). A library one want
// larger than reconcileBatch used to have its newest want left out of every
// pass, because each pass re-read the same oldest page.
func TestTheReconcileSweepReachesWantsPastTheFirstBatch(t *testing.T) {
	t.Parallel()
	h := newSearchHarness(t)

	// reconcileBatch wants older than the harness's own, so the harness want
	// is the (reconcileBatch+1)th — the first one past the old cap.
	// A work each: one want per target and profile.
	h.exec(t, `
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO works
			(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		SELECT printf('w-old-%06d', i), 'movie', printf('movie:old-%06d:2000', i), 'Old', 'old', 2000, '{}',
		       '2000-01-01T00:00:00Z', '2000-01-01T00:00:00Z'
		FROM n`, reconcileBatch)
	h.exec(t, `
		INSERT INTO desired_items
			(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		SELECT substr(id, 3), 'work', id, NULL, 'q1', 1, '', created_at, created_at
		FROM works WHERE id LIKE 'w-old-%'`)
	h.exec(t, `
		INSERT INTO acquisition_state (desired_item_id, phase_entered_at, created_at, updated_at)
		SELECT id, created_at, created_at, created_at FROM desired_items WHERE id LIKE 'old-%'`)

	var before string
	if err := h.db.Reader().QueryRow(`SELECT content FROM acquisition_state WHERE desired_item_id = ?`,
		h.want).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != "unknown" {
		t.Fatalf("setup: the newest want's content is %q, want unknown before any sweep", before)
	}

	handler := ReconcileHandler(h.cat, slog.New(slog.DiscardHandler))
	if err := handler(t.Context(), jobs.Job{Type: acquisition.ReconcileJobType}); err != nil {
		t.Fatal(err)
	}

	var after string
	if err := h.db.Reader().QueryRow(`SELECT content FROM acquisition_state WHERE desired_item_id = ?`,
		h.want).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after == "unknown" {
		t.Errorf("the want past the first %d was never reconciled — the sweep stopped at one page", reconcileBatch)
	}
}
