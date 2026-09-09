package catalog_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// The subtitle fetch queries (ADR-0085): which wants are due a provider fetch,
// which video a fetched subtitle attaches to, and that a fetched subtitle
// satisfies the want.

func hexHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "blake3:" + hex.EncodeToString(sum[:])
}

// seedEpisodeVideo attaches a held (managed, blobbed) primary video for the
// episode item on the season edition, linked to the item (ADR-0086), and gives
// the work an imdb external id and the item a season/episode. It builds on
// seedSubtitleWant (which created e-season, it-e01, want-sub).
func seedEpisodeVideo(t *testing.T, h *harness, itemID string) {
	t.Helper()
	// RecordFetchedSubtitle writes a replica against the self peer, which the
	// real ingest path creates; make it exist here too.
	if _, err := h.cat.SelfPeer(context.Background()); err != nil {
		t.Fatal(err)
	}
	vhash := hexHash("video-" + itemID)
	h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at)
		VALUES (?, 1000000000, 'video/x-matroska', ?)`, vhash, stamp)
	h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
			source_path, role, filename, mime, identification_source, item_id, created_at, updated_at)
		VALUES ('v-e01', 'e-season', NULL, 'managed', ?, '/srv/Show.S01E01.mkv', 'primary',
			'Show.S01E01.mkv', 'video/x-matroska', 'path', ?, ?, ?)`, vhash, itemID, stamp, stamp)
	h.exec(t, `INSERT INTO external_ids (id, entity_type, entity_id, source, value)
		VALUES ('x-imdb', 'work', 'w1', 'imdb', 'tt0944947')`)
	h.exec(t, `UPDATE items SET attributes = '{"season":"1","episode":"1"}' WHERE id = ?`, itemID)
}

func TestRecordFetchedSubtitleAttachesToVideoEdition(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, itemID := seedSubtitleWant(t, h)
	seedEpisodeVideo(t, h, itemID)

	sub := catalog.FetchedSubtitle{BlobHash: hexHash("sub-en"), Size: 4096, Language: "en"}
	if err := h.cat.RecordFetchedSubtitle(ctx, "v-e01", sub, time.Now().UTC()); err != nil {
		t.Fatalf("record: %v", err)
	}

	var editionID, role, lang, item, filename string
	if err := h.db.Reader().QueryRowContext(ctx, `
		SELECT edition_id, role, coalesce(json_extract(attributes,'$.language'),''),
		       coalesce(item_id,''), coalesce(filename,'')
		FROM assets WHERE blob_hash = ?`, sub.BlobHash).
		Scan(&editionID, &role, &lang, &item, &filename); err != nil {
		t.Fatal(err)
	}
	if editionID != "e-season" || role != "subtitle" {
		t.Errorf("attached to edition %q as %q, want e-season/subtitle", editionID, role)
	}
	if lang != "en" {
		t.Errorf("attributes.language = %q, want en", lang)
	}
	if item != itemID {
		t.Errorf("item_id = %q, want %q (carried from the source video)", item, itemID)
	}
	// Stem-matches the video so the caption resolver binds it.
	if filename != "Show.S01E01.en.srt" {
		t.Errorf("filename = %q, want Show.S01E01.en.srt", filename)
	}
}

func TestFetchedSubtitleSatisfiesTheWant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	want, itemID := seedSubtitleWant(t, h)
	seedEpisodeVideo(t, h, itemID)

	sub := catalog.FetchedSubtitle{BlobHash: hexHash("sub-en2"), Size: 4096, Language: "en"}
	if err := h.cat.RecordFetchedSubtitle(ctx, "v-e01", sub, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err := h.cat.ReconcileDesired(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content.Satisfaction != acquisition.SatisfactionSatisfied {
		t.Fatalf("content = %s, want satisfied after a fetched subtitle", got.Content.Satisfaction)
	}
}

func TestDueSubtitleFetchesRequiresHeldVideoAndExternalID(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	want, itemID := seedSubtitleWant(t, h)

	// No video held yet → not due (you cannot caption a video you do not have).
	due, err := h.cat.DueSubtitleFetches(ctx, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("due = %d before the video is held, want 0", len(due))
	}

	// Hold the video + give the work an id → due.
	seedEpisodeVideo(t, h, itemID)
	due, err = h.cat.DueSubtitleFetches(ctx, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].DesiredItemID != want {
		t.Fatalf("due = %+v, want just %s", due, want)
	}
	d := due[0]
	if d.IMDBID != "0944947" { // the "tt" prefix is stripped
		t.Errorf("imdb id = %q, want 0944947", d.IMDBID)
	}
	if d.Season != 1 || d.Episode != 1 {
		t.Errorf("season/episode = %d/%d, want 1/1", d.Season, d.Episode)
	}
	if d.Language != "en" {
		t.Errorf("language = %q, want en", d.Language)
	}
}

func TestDueSubtitleFetchesSkipsSatisfiedAndNonSubtitle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, itemID := seedSubtitleWant(t, h)
	seedEpisodeVideo(t, h, itemID)

	// The default harness want (work-scoped, primary aspect) must never be due.
	due, err := h.cat.DueSubtitleFetches(ctx, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.DesiredItemID == h.want {
			t.Fatal("a primary want was due a subtitle fetch")
		}
	}

	// Once satisfied, a subtitle want leaves the due set.
	sub := catalog.FetchedSubtitle{BlobHash: hexHash("sub-en3"), Size: 4096, Language: "en"}
	if err := h.cat.RecordFetchedSubtitle(ctx, "v-e01", sub, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.ReconcileDesired(ctx, "want-sub"); err != nil {
		t.Fatal(err)
	}
	due, err = h.cat.DueSubtitleFetches(ctx, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("due = %d after satisfaction, want 0", len(due))
	}
}

func TestSourceVideoForSubtitlePrefersItemMatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, itemID := seedSubtitleWant(t, h)
	seedEpisodeVideo(t, h, itemID)

	got, ok, err := h.cat.SourceVideoForSubtitle(ctx, "want-sub")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != "v-e01" {
		t.Fatalf("source video = %q ok=%v, want v-e01", got, ok)
	}
}
