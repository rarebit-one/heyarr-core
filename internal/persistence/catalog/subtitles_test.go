package catalog_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

type subClock struct{ t time.Time }

func (c *subClock) Now() time.Time { return c.t }

// The blobs table enforces hash = "blake3:" + 64 hex, so test hashes must be
// well-formed even though the bytes are fictional.
var (
	videoBlob = "blake3:" + strings.Repeat("a", 64)
	subENBlob = "blake3:" + strings.Repeat("b", 64)
	subFRBlob = "blake3:" + strings.Repeat("c", 64)
)

// seedVideo stands up the minimum a RecordExtractedSubtitle needs: a self peer,
// a work, an edition, and a managed video asset on that edition. Raw SQL rather
// than the full ingest/library machinery on purpose — this tests the recorder's
// behaviour, not the scan path.
func seedVideo(t *testing.T) (cat *catalog.Catalog, ctx context.Context, db *sqlite.DB, sourceAssetID, editionID string) {
	t.Helper()
	ctx = context.Background()
	db = testdb.Migrated(t)
	clock := &subClock{t: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	cat, err = catalog.New(catalog.Options{DB: db, Events: log, PeerName: "node-a", PeerSite: "site-a", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	// RecordExtractedSubtitle writes a replica against the self peer, which the
	// real ingest path creates; make it exist here too.
	if _, err := cat.SelfPeer(ctx); err != nil {
		t.Fatal(err)
	}

	stamp := clock.t.Format(time.RFC3339)
	editionID = "ed-1"
	sourceAssetID = "asset-video-1"
	w := db.Writer()
	exec := func(q string, args ...any) {
		if _, err := w.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, created_at, updated_at)
	      VALUES ('work-1','series','yellowstone','Yellowstone','yellowstone',?,?)`, stamp, stamp)
	exec(`INSERT INTO editions (id, work_id, created_at) VALUES (?, 'work-1', ?)`, editionID, stamp)
	exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1000, 'video/mp4', ?)`, videoBlob, stamp)
	exec(`INSERT INTO assets (id, edition_id, source_class, blob_hash, source_path, role, filename, mime, identification_source, created_at, updated_at)
	      VALUES (?, ?, 'managed', ?, '/lib/Yellowstone S05E01.mp4', 'primary', 'Yellowstone S05E01.mp4', 'video/mp4', 'scan', ?, ?)`,
		sourceAssetID, editionID, videoBlob, stamp, stamp)
	return cat, ctx, db, sourceAssetID, editionID
}

func TestRecordExtractedSubtitleLandsAsACaptionAsset(t *testing.T) {
	t.Parallel()
	cat, ctx, db, sourceAssetID, editionID := seedVideo(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	if err := cat.RecordExtractedSubtitle(ctx, sourceAssetID, catalog.ExtractedSubtitle{
		BlobHash: subENBlob, Size: 512, Language: "en",
	}, now); err != nil {
		t.Fatalf("RecordExtractedSubtitle: %v", err)
	}

	var (
		gotEdition, role, mime, filename, attrs string
		sourcePath                              sql.NullString
	)
	err := db.Reader().QueryRowContext(ctx,
		`SELECT edition_id, role, mime, filename, source_path, attributes
		 FROM assets WHERE role = 'subtitle'`).
		Scan(&gotEdition, &role, &mime, &filename, &sourcePath, &attrs)
	if err != nil {
		t.Fatalf("reading the extracted subtitle asset: %v", err)
	}
	if gotEdition != editionID {
		t.Errorf("edition = %q, want the video's %q", gotEdition, editionID)
	}
	if role != "subtitle" {
		t.Errorf("role = %q, want subtitle (so the caption resolver selects it)", role)
	}
	if mime != "application/x-subrip" {
		t.Errorf("mime = %q, want application/x-subrip (a whitelisted caption type)", mime)
	}
	if sourcePath.Valid {
		t.Errorf("source_path = %q, want NULL — an extracted subtitle has no on-disk file", sourcePath.String)
	}
	// The stem must stay a PREFIX of the video's stem or the resolver won't bind
	// it: video stem "Yellowstone S05E01", subtitle "Yellowstone S05E01.en.srt".
	if filename != "Yellowstone S05E01.en.srt" {
		t.Errorf("filename = %q, want Yellowstone S05E01.en.srt", filename)
	}
	if !strings.Contains(attrs, `"language":"en"`) {
		t.Errorf("attributes = %q, want it to carry the language", attrs)
	}
	if !strings.Contains(attrs, `"source":"embedded"`) {
		t.Errorf("attributes = %q, want it to record the embedded origin", attrs)
	}
}

func TestRecordExtractedSubtitleIsIdempotentButKeepsDistinctTracks(t *testing.T) {
	t.Parallel()
	cat, ctx, db, sourceAssetID, _ := seedVideo(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	// The job re-runs (invariant 9): the same track (same blob) must converge on
	// one row, not accumulate.
	for range 3 {
		if err := cat.RecordExtractedSubtitle(ctx, sourceAssetID, catalog.ExtractedSubtitle{
			BlobHash: subENBlob, Size: 512, Language: "en",
		}, now); err != nil {
			t.Fatal(err)
		}
	}
	// A genuinely different track (different bytes → different blob) is its own
	// subtitle and gets its own row.
	if err := cat.RecordExtractedSubtitle(ctx, sourceAssetID, catalog.ExtractedSubtitle{
		BlobHash: subFRBlob, Size: 400, Language: "fr",
	}, now); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM assets WHERE role = 'subtitle'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("subtitle asset count = %d, want 2 (one per distinct track, re-runs converge)", n)
	}
}

func TestEmbeddedEnglishSdhUsesCanonicalLanguageAndSdhFilename(t *testing.T) {
	cat, ctx, db, source, _ := seedVideo(t)
	if err := cat.RecordExtractedSubtitle(ctx, source, catalog.ExtractedSubtitle{BlobHash: subENBlob, Size: 512, Language: "eng", HearingImpaired: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var filename, attrs string
	if err := db.Reader().QueryRowContext(ctx, `SELECT filename,attributes FROM assets WHERE role='subtitle'`).Scan(&filename, &attrs); err != nil {
		t.Fatal(err)
	}
	if filename != "Yellowstone S05E01.en.sdh.srt" || !strings.Contains(attrs, `"language":"en"`) || !strings.Contains(attrs, `"hearing_impaired":true`) {
		t.Fatalf("filename=%s attrs=%s", filename, attrs)
	}
}

func TestSubtitleLanguageDoesNotInferUnknownOrDiscardRegion(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"", ""}, {"und", ""}, {"und-Latn", ""}, {"eng", "en"}, {"zh-Hant", "zh-Hant"}, {"pt-BR", "pt-BR"}} {
		name := tc.input
		if name == "" {
			name = "untagged"
		}
		t.Run(name, func(t *testing.T) {
			cat, ctx, db, source, _ := seedVideo(t)
			if err := cat.RecordExtractedSubtitle(ctx, source, catalog.ExtractedSubtitle{BlobHash: subENBlob, Size: 512, Language: tc.input}, time.Now()); err != nil {
				t.Fatal(err)
			}
			var filename, attrs string
			if err := db.Reader().QueryRowContext(ctx, `SELECT filename,attributes FROM assets WHERE role='subtitle'`).Scan(&filename, &attrs); err != nil {
				t.Fatal(err)
			}
			var metadata map[string]any
			if err := json.Unmarshal([]byte(attrs), &metadata); err != nil {
				t.Fatal(err)
			}
			got, _ := metadata["language"].(string)
			if got != tc.want {
				t.Fatalf("language=%q want=%q", got, tc.want)
			}
			wantName := "Yellowstone S05E01"
			if tc.want != "" {
				wantName += "." + tc.want
			}
			if filename != wantName+".srt" {
				t.Fatalf("filename=%q want=%q", filename, wantName+".srt")
			}
		})
	}
}
