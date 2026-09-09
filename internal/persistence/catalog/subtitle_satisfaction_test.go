package catalog_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
)

// Subtitle-aspect satisfaction (ADR-0085/0086): a subtitle want is satisfied by
// a role='subtitle' asset of the want's language on the PRECISE target it points
// at — matched by item_id for a series episode, so one episode's caption does
// not satisfy another on the same season edition.

const subtitleProfile = `[{"attribute":"size_bytes","op":"gte","value":1}]`

// seedSubtitleWant stands up a season edition, an episode item, a `subtitle`
// quality profile, and an item-scoped subtitle want for English, then starts its
// acquisition. Returns the want id and the item id.
func seedSubtitleWant(t *testing.T, h *harness) (wantID, itemID string) {
	t.Helper()
	h.exec(t, `INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q-sub', 'subtitle', '', ?, '[]', ?, 1, ?, ?)`,
		subtitleProfile, subtitleProfile, stamp, stamp)
	h.exec(t, `INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at)
		VALUES ('e-season', 'w1', 'Season 1', 'web-dl', 'en', '{}', ?)`, stamp)
	h.exec(t, `INSERT INTO items (id, work_id, edition_id, item_key, title, attributes, created_at, updated_at)
		VALUES ('it-e01', 'w1', 'e-season', 'S01E01', 'Pilot', '{}', ?, ?)`, stamp, stamp)
	h.exec(t, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, item_id, aspect, language, quality_profile_id,
		 monitor, reason, created_at, updated_at)
		VALUES ('want-sub', 'item', 'w1', NULL, 'it-e01', 'subtitle', 'en', 'q-sub',
		 1, '', ?, ?)`, stamp, stamp)

	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, "want-sub"); err != nil {
		t.Fatal(err)
	}
	return "want-sub", "it-e01"
}

// seedSubtitleAsset attaches a subtitle asset to the season edition, with a
// chosen item link, language (in the attributes) and filename.
func seedSubtitleAsset(t *testing.T, h *harness, id, itemID, attrs, filename string) {
	t.Helper()
	sum := sha256.Sum256([]byte(id))
	hash := "blake3:" + hex.EncodeToString(sum[:]) // 64 hex chars, distinct per id
	h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at)
		VALUES (?, 4096, 'application/x-subrip', ?)`, hash, stamp)
	var item any
	if itemID != "" {
		item = itemID
	}
	h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
			source_path, role, filename, mime, identification_source, attributes, item_id,
			created_at, updated_at)
		VALUES (?, 'e-season', NULL, 'managed', ?, NULL, 'subtitle', ?, 'application/x-subrip',
			'fetched', ?, ?, ?, ?)`, id, hash, filename, attrs, item, stamp, stamp)
}

func TestSubtitleWantSatisfiedByMatchingLanguageAndItem(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	want, itemID := seedSubtitleWant(t, h)

	// An English subtitle for THIS episode.
	seedSubtitleAsset(t, h, "a-sub-en", itemID, `{"language":"en"}`, "Show.S01E01.en.srt")

	got, err := h.cat.ReconcileDesired(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content.Satisfaction != acquisition.SatisfactionSatisfied {
		t.Fatalf("content = %s, want satisfied", got.Content.Satisfaction)
	}
}

func TestSubtitleWantNotSatisfiedByWrongLanguage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	want, itemID := seedSubtitleWant(t, h)

	// A German subtitle for the episode does not satisfy an English want.
	seedSubtitleAsset(t, h, "a-sub-de", itemID, `{"language":"de"}`, "Show.S01E01.de.srt")

	got, err := h.cat.ReconcileDesired(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content.Satisfaction == acquisition.SatisfactionSatisfied {
		t.Fatal("a German subtitle satisfied an English want")
	}
}

func TestSubtitleWantNotSatisfiedByAnotherEpisodesSubtitle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	want, _ := seedSubtitleWant(t, h)

	// An English subtitle on the SAME season edition but linked to a different
	// episode must not satisfy this episode's want — the whole point of ADR-0086.
	h.exec(t, `INSERT INTO items (id, work_id, edition_id, item_key, title, attributes, created_at, updated_at)
		VALUES ('it-e02', 'w1', 'e-season', 'S01E02', 'Two', '{}', ?, ?)`, stamp, stamp)
	seedSubtitleAsset(t, h, "a-sub-e02", "it-e02", `{"language":"en"}`, "Show.S01E02.en.srt")

	got, err := h.cat.ReconcileDesired(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content.Satisfaction == acquisition.SatisfactionSatisfied {
		t.Fatal("another episode's subtitle satisfied this episode's want")
	}
}

func TestSubtitleWantSatisfiedByFilenameLanguageFallback(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	want, itemID := seedSubtitleWant(t, h)

	// A subtitle whose language never reached the attributes but follows the
	// sidecar filename convention is still matched.
	seedSubtitleAsset(t, h, "a-sub-fn", itemID, `{}`, "Show.S01E01.en.srt")

	got, err := h.cat.ReconcileDesired(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content.Satisfaction != acquisition.SatisfactionSatisfied {
		t.Fatalf("content = %s, want satisfied via filename fallback", got.Content.Satisfaction)
	}
}

func TestSetAssetItemLinksAndClears(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, itemID := seedSubtitleWant(t, h)
	seedSubtitleAsset(t, h, "a-link", "", `{"language":"en"}`, "Show.S01E01.en.srt")

	if err := h.cat.SetAssetItem(ctx, "a-link", itemID); err != nil {
		t.Fatalf("link: %v", err)
	}
	var linked string
	if err := h.db.Reader().QueryRowContext(ctx,
		`SELECT coalesce(item_id, '') FROM assets WHERE id = 'a-link'`).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != itemID {
		t.Fatalf("item_id = %q, want %q", linked, itemID)
	}

	// Empty clears the link (a work/edition-scoped grab).
	if err := h.cat.SetAssetItem(ctx, "a-link", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := h.db.Reader().QueryRowContext(ctx,
		`SELECT coalesce(item_id, '') FROM assets WHERE id = 'a-link'`).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked != "" {
		t.Fatalf("item_id = %q, want cleared", linked)
	}
}

// A subtitle want is direct-route, so the search beat never picks it up whatever
// its state (ADR-0085): DueSearches must not return it.
func TestDueSearchesSkipsSubtitleWants(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	want, _ := seedSubtitleWant(t, h)

	due, err := h.cat.DueSearches(ctx, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.DesiredItemID == want {
			t.Fatal("a subtitle want was scheduled for an indexer search")
		}
	}
}
