package catalog_test

import (
	"context"
	"testing"
	"time"
)

// The subtitle fetch and enrich schedules share the search schedule's
// fruitless-backoff bookkeeping (backoffschedule.go). The search half has its
// own tests; these pin the other two: a recorded attempt carries its streak and
// takes the subject out of the due set until its next-due time, and — unlike the
// search schedule's compare-and-set — a later record always overwrites.

func TestSubtitleFetchScheduleBacksOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	want, itemID := seedSubtitleWant(t, h)
	seedEpisodeVideo(t, h, itemID)
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	due := func(at time.Time) int {
		t.Helper()
		got, err := h.cat.DueSubtitleFetches(ctx, at, 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if due(now) != 1 {
		t.Fatal("setup: a never-fetched subtitle want must be due")
	}

	if err := h.cat.RecordSubtitleFetchScheduled(ctx, want, 2, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := due(now); n != 0 {
		t.Fatalf("due = %d an hour before the next fetch, want 0", n)
	}
	if n := due(now.Add(time.Hour)); n != 1 {
		t.Fatalf("due = %d at the next fetch, want 1", n)
	}
	fc, ok, err := h.cat.SubtitleFetchContext(ctx, want)
	if err != nil || !ok {
		t.Fatalf("fetch context: ok=%v err=%v", ok, err)
	}
	if fc.Fruitless != 2 {
		t.Errorf("fruitless = %d, want 2", fc.Fruitless)
	}

	// Not a compare-and-set: recording again before the row is due still lands.
	if err := h.cat.RecordSubtitleFetchScheduled(ctx, want, 5, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := due(now.Add(time.Minute)); n != 1 {
		t.Fatalf("due = %d after an overwriting record, want 1", n)
	}
	if fc, _, _ := h.cat.SubtitleFetchContext(ctx, want); fc.Fruitless != 5 {
		t.Errorf("fruitless = %d after an overwriting record, want 5", fc.Fruitless)
	}
}

func TestEnrichScheduleBacksOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	h.exec(t, `INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w-album', 'music', 'music:an-album', 'An Album', 'an album', 2001,
			'{"artist":"An Artist"}', ?, ?)`, stamp, stamp)
	h.exec(t, `INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at)
		VALUES ('e-album', 'w-album', 'flac', 'cd', '', '{}', ?)`, stamp)
	track := hexHash("track-1")
	h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1000, 'audio/flac', ?)`,
		track, stamp)
	h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
			source_path, role, filename, mime, identification_source, created_at, updated_at)
		VALUES ('a-track', 'e-album', NULL, 'managed', ?, '/srv/media/music/01.flac', 'primary',
			'01.flac', 'audio/flac', 'path', ?, ?)`, track, stamp, stamp)
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	due := func(at time.Time) int {
		t.Helper()
		got, err := h.cat.DueEnrichWorks(ctx, at, 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if due(now) != 1 {
		t.Fatal("setup: a never-enriched, held, bare music work must be due")
	}

	if err := h.cat.RecordEnrichScheduled(ctx, "w-album", 3, now, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := due(now); n != 0 {
		t.Fatalf("due = %d a day before the next pass, want 0", n)
	}
	if n := due(now.Add(24 * time.Hour)); n != 1 {
		t.Fatalf("due = %d at the next pass, want 1", n)
	}
	d, ok, err := h.cat.EnrichContext(ctx, "w-album")
	if err != nil || !ok {
		t.Fatalf("enrich context: ok=%v err=%v", ok, err)
	}
	if d.Fruitless != 3 || d.Artist != "An Artist" {
		t.Errorf("context = %+v, want fruitless 3 and the artist", d)
	}

	// Not a compare-and-set: recording again before the row is due still lands.
	if err := h.cat.RecordEnrichScheduled(ctx, "w-album", 0, now, now); err != nil {
		t.Fatal(err)
	}
	if n := due(now); n != 1 {
		t.Fatalf("due = %d after an overwriting record, want 1", n)
	}
}
