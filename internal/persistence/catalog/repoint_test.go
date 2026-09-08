package catalog_test

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// The in-place repoint door (ADR-0082): a subscription's strategy — profile,
// backfill, or both — changes without an unfollow, and the two halves have
// different follow-ups. A profile change moves the wants and hands their ids
// back for a reconcile; a backfill change moves nothing but the source, because
// what it changes is what the NEXT poll projects.

// seedSubscription gives the harness one followed source over its movie work,
// a second profile to move to, and one item-scoped want the source has
// projected — the minimum a repoint has to move.
func seedSubscription(t *testing.T, h *harness) (sourceID, wantID string) {
	t.Helper()
	h.exec(t, `INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q2', 'published', '', '[]', '[]', '[]', 1, ?, ?)`, stamp, stamp)
	h.exec(t, `INSERT INTO follow_sources
		(id, work_id, type, feed_ref, quality_profile_id, backfill, created_at, updated_at)
		VALUES ('src-1', 'w1', 'rss_feed', 'https://example.test/feed', 'q1', 'from_now', ?, ?)`,
		stamp, stamp)
	h.exec(t, `INSERT INTO items (id, work_id, item_key, created_at, updated_at)
		VALUES ('item-1', 'w1', 'https://example.test/?p=1', ?, ?)`, stamp, stamp)
	h.exec(t, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, item_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES ('want-item-1', 'item', 'w1', NULL, 'item-1', 'q1', 1, '', ?, ?)`, stamp, stamp)
	return "src-1", "want-item-1"
}

func wantProfile(t *testing.T, h *harness, wantID string) string {
	t.Helper()
	var p string
	if err := h.db.Reader().QueryRowContext(t.Context(),
		`SELECT quality_profile_id FROM desired_items WHERE id = ?`, wantID).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRepointingTheProfileMovesTheSourceAndItsWants(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	src, want := seedSubscription(t, h)

	moved, err := h.cat.RepointFollowedSource(ctx, src, "q2", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 1 || moved[0] != want {
		t.Fatalf("moved = %v, want exactly [%s] — the item-scoped want this source projects", moved, want)
	}
	s, err := h.cat.FollowSource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if s.QualityProfileID != "q2" {
		t.Errorf("source profile = %q, want q2", s.QualityProfileID)
	}
	if string(s.Backfill) != "from_now" {
		t.Errorf("backfill = %q; a profile-only repoint must leave it alone", s.Backfill)
	}
	if got := wantProfile(t, h, want); got != "q2" {
		t.Errorf("want profile = %q, want q2 — the source and its wants must move together, "+
			"or the subscription means two standards at once", got)
	}
	// The harness's own WORK-scoped want is not this subscription's and must not
	// have been touched.
	if got := wantProfile(t, h, h.want); got != "q1" {
		t.Errorf("the unrelated work-scoped want moved to %q; only item-scoped wants belong to a source", got)
	}
}

func TestRepointingTheBackfillMovesOnlyTheSource(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	src, want := seedSubscription(t, h)

	moved, err := h.cat.RepointFollowedSource(ctx, src, "", "full")
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 0 {
		t.Fatalf("moved = %v, want none — a backfill change is felt by the next poll, not by a reconcile", moved)
	}
	s, err := h.cat.FollowSource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if string(s.Backfill) != "full" {
		t.Errorf("backfill = %q, want full", s.Backfill)
	}
	if s.QualityProfileID != "q1" {
		t.Errorf("profile = %q; a backfill-only repoint must leave it alone", s.QualityProfileID)
	}
	if got := wantProfile(t, h, want); got != "q1" {
		t.Errorf("want profile = %q; a backfill change must not touch wants", got)
	}
}

func TestRepointingBothMovesBoth(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	src, want := seedSubscription(t, h)

	moved, err := h.cat.RepointFollowedSource(ctx, src, "q2", "full")
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 1 {
		t.Fatalf("moved %d wants, want 1", len(moved))
	}
	s, _ := h.cat.FollowSource(ctx, src)
	if s.QualityProfileID != "q2" || string(s.Backfill) != "full" {
		t.Errorf("source = (%s, %s), want (q2, full)", s.QualityProfileID, s.Backfill)
	}
	if got := wantProfile(t, h, want); got != "q2" {
		t.Errorf("want profile = %q, want q2", got)
	}
}

func TestARepointThatChangesNothingIsRefused(t *testing.T) {
	h := newHarness(t)
	src, _ := seedSubscription(t, h)
	if _, err := h.cat.RepointFollowedSource(t.Context(), src, "", ""); err == nil {
		t.Fatal("a repoint naming neither a profile nor a backfill was accepted; it must be refused " +
			"rather than silently touching updated_at and nothing else")
	}
}

func TestRepointingAMissingSourceIsNotFound(t *testing.T) {
	h := newHarness(t)
	_, err := h.cat.RepointFollowedSource(t.Context(), "nope", "q1", "")
	if err == nil || err.Error() != catalog.ErrNoFollowSource.Error() {
		t.Fatalf("err = %v, want ErrNoFollowSource", err)
	}
}
