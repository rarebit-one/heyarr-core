package worker

import (
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
)

// A followed source with want_subtitles projects, beside each episode's primary
// want, a subtitle want per language (ADR-0085 §6) — idempotent on re-poll.

// countWantsByAspect returns how many item-scoped wants exist for each aspect,
// and how many subtitle wants carry the given profile+language.
func (h *followHarness) countWantsByAspect(t *testing.T) (primary, subtitle int) {
	t.Helper()
	rows, err := h.db.Reader().Query(
		`SELECT aspect, count(*) FROM desired_items WHERE scope = 'item' GROUP BY aspect`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var aspect string
		var n int
		if err := rows.Scan(&aspect, &n); err != nil {
			t.Fatal(err)
		}
		switch aspect {
		case "primary":
			primary = n
		case "subtitle":
			subtitle = n
		}
	}
	return primary, subtitle
}

func TestPollProjectsSubtitleWantsWhenSourceWantsThem(t *testing.T) {
	h := newFollowHarness(t, followed.BackfillFull)

	// The seeded subtitle profile the projection resolves by name.
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	h.exec(t, `INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q-sub', 'subtitle', '', '[{"attribute":"size_bytes","op":"gte","value":1}]',
			'[]', '[{"attribute":"size_bytes","op":"gte","value":1}]', 1, ?, ?)`, stamp, stamp)

	// Turn the subscription's subtitle intent on: English captions for every episode.
	langs := []string{"en"}
	if _, err := h.cat.RepointFollowedSource(t.Context(), h.sourceID, "", "", &langs); err != nil {
		t.Fatalf("set want_subtitles: %v", err)
	}

	aired := time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC)
	h.feed.OfferFeed(h.feedRef,
		episode("S02E01", "The Return", aired),
		episode("S02E02", "The Reckoning", aired.AddDate(0, 0, 7)))

	if err := h.poll(t); err != nil {
		t.Fatalf("poll: %v", err)
	}

	primary, subtitle := h.countWantsByAspect(t)
	if primary != 2 {
		t.Errorf("primary wants = %d, want 2", primary)
	}
	if subtitle != 2 {
		t.Errorf("subtitle wants = %d, want 2 (one en subtitle per episode)", subtitle)
	}

	// Each subtitle want is en, on the subtitle profile, item-scoped.
	rows, err := h.db.Reader().Query(
		`SELECT language, quality_profile_id, monitor FROM desired_items WHERE aspect = 'subtitle'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var lang, profile string
		var monitor int
		if err := rows.Scan(&lang, &profile, &monitor); err != nil {
			t.Fatal(err)
		}
		if lang != "en" || profile != "q-sub" || monitor != 0 {
			t.Errorf("subtitle want = lang %q profile %q monitor %d, want en/q-sub/0", lang, profile, monitor)
		}
	}

	// A re-poll must not duplicate — the wants are idempotent (invariant 9).
	if err := h.poll(t); err != nil {
		t.Fatalf("re-poll: %v", err)
	}
	primary, subtitle = h.countWantsByAspect(t)
	if primary != 2 || subtitle != 2 {
		t.Errorf("after re-poll: primary=%d subtitle=%d, want 2 and 2", primary, subtitle)
	}
}

// With no want_subtitles set, a poll projects only primary wants — the feature is
// off by default and costs nothing.
func TestPollProjectsNoSubtitleWantsByDefault(t *testing.T) {
	h := newFollowHarness(t, followed.BackfillFull)
	h.feed.OfferFeed(h.feedRef, episode("S02E01", "The Return", time.Now().UTC()))
	if err := h.poll(t); err != nil {
		t.Fatalf("poll: %v", err)
	}
	primary, subtitle := h.countWantsByAspect(t)
	if primary != 1 || subtitle != 0 {
		t.Errorf("primary=%d subtitle=%d, want 1 and 0", primary, subtitle)
	}
}
