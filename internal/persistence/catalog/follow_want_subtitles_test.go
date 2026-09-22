package catalog_test

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
)

// A followed source's wanted subtitle languages round-trip through the database
// and can be changed in place (ADR-0085 §6).

func TestFollowSourceRoundTripsWantSubtitles(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	src := source("w1", followed.BackfillFromNow)
	src.WantSubtitles = []string{"EN", "en", "", "de"} // normalised on Validate
	created, err := h.cat.CreateFollowSource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}

	got, err := h.cat.FollowSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.WantSubtitles) != 2 || got.WantSubtitles[0] != "en" || got.WantSubtitles[1] != "de" {
		t.Fatalf("want_subtitles = %v, want [en de]", got.WantSubtitles)
	}
}

func TestRepointChangesWantSubtitlesInPlace(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	created, err := h.cat.CreateFollowSource(ctx, source("w1", followed.BackfillFromNow))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := h.cat.FollowSource(ctx, created.ID); len(got.WantSubtitles) != 0 {
		t.Fatalf("a new source wants no subtitles, got %v", got.WantSubtitles)
	}

	// Turn it on.
	langs := []string{"en", "fr"}
	if _, err := h.cat.RepointFollowedSource(ctx, created.ID, "", "", &langs); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.cat.FollowSource(ctx, created.ID); len(got.WantSubtitles) != 2 {
		t.Fatalf("after set, want_subtitles = %v, want [en fr]", got.WantSubtitles)
	}

	// A repoint that does not name languages (nil) leaves them alone.
	if _, err := h.cat.RepointFollowedSource(ctx, created.ID, "", "full", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.cat.FollowSource(ctx, created.ID); len(got.WantSubtitles) != 2 {
		t.Errorf("a backfill-only repoint changed the languages: %v", got.WantSubtitles)
	}

	// An explicit empty set clears them.
	empty := []string{}
	if _, err := h.cat.RepointFollowedSource(ctx, created.ID, "", "", &empty); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.cat.FollowSource(ctx, created.ID); len(got.WantSubtitles) != 0 {
		t.Errorf("an explicit empty set did not clear the languages: %v", got.WantSubtitles)
	}
}
