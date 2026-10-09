package catalog_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/media/cover"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

func blobOf(c byte) string { return "blake3:" + strings.Repeat(string(c), 64) }

type bookSeed struct {
	cat   *catalog.Catalog
	ctx   context.Context
	db    *sqlite.DB
	stamp string
	t     *testing.T
}

func newBookSeed(t *testing.T) *bookSeed {
	t.Helper()
	db := testdb.Migrated(t)
	clock := &subClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{DB: db, Events: log, PeerName: "node-a", PeerSite: "site-a", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := cat.SelfPeer(ctx); err != nil {
		t.Fatal(err)
	}
	return &bookSeed{cat: cat, ctx: ctx, db: db, stamp: clock.t.Format(time.RFC3339), t: t}
}

func (s *bookSeed) exec(q string, args ...any) {
	s.t.Helper()
	if _, err := s.db.Writer().ExecContext(s.ctx, q, args...); err != nil {
		s.t.Fatalf("seed %q: %v", q, err)
	}
}

// book seeds a Work of the given content type with one Edition holding one
// managed primary asset, and returns the asset id.
func (s *bookSeed) book(id, contentType, mime, blob string) string {
	s.t.Helper()
	s.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, created_at, updated_at)
	        VALUES (?, ?, ?, ?, ?, ?, ?)`, "w-"+id, contentType, "k-"+id, id, id, s.stamp, s.stamp)
	s.exec(`INSERT INTO editions (id, work_id, created_at) VALUES (?, ?, ?)`, "e-"+id, "w-"+id, s.stamp)
	s.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1000, ?, ?) ON CONFLICT DO NOTHING`, blob, mime, s.stamp)
	s.exec(`INSERT INTO assets (id, edition_id, source_class, blob_hash, source_path, role, filename, mime, identification_source, created_at, updated_at)
	        VALUES (?, ?, 'managed', ?, ?, 'primary', ?, ?, 'scan', ?, ?)`,
		"a-"+id, "e-"+id, blob, "/lib/"+id, id, mime, s.stamp, s.stamp)
	return "a-" + id
}

// artwork attaches an artwork asset to the book's Edition with the given
// identification_source ('scan' for a shipped cover.jpg, 'fetched' for Open
// Library's).
func (s *bookSeed) artwork(id, blob, source string) {
	s.t.Helper()
	s.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 10, 'image/jpeg', ?)`, blob, s.stamp)
	s.exec(`INSERT INTO assets (id, edition_id, source_class, blob_hash, role, filename, mime, identification_source, created_at, updated_at)
	        VALUES (?, ?, 'managed', ?, 'artwork', 'cover.jpg', 'image/jpeg', ?, ?, ?)`,
		"art-"+id, "e-"+id, blob, source, s.stamp, s.stamp)
}

func dueIDs(t *testing.T, s *bookSeed) []string {
	t.Helper()
	due, err := s.cat.DueCoverExtractions(s.ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range due {
		ids = append(ids, d.AssetID)
	}
	return ids
}

func TestDueCoverExtractionsChoosesBookFilesWithoutTheirOwnCover(t *testing.T) {
	t.Parallel()
	s := newBookSeed(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	s.book("pdf", "book", "application/pdf", blobOf('1'))
	s.book("epub", "book", "application/epub+zip", blobOf('2'))
	s.book("mobi", "book", "application/x-mobipocket-ebook", blobOf('3'))
	s.book("doc", "document", "application/pdf", blobOf('4'))
	s.book("shipped", "book", "application/pdf", blobOf('5'))
	s.artwork("shipped", blobOf('a'), "scan")
	s.book("fetched", "book", "application/pdf", blobOf('6'))
	s.artwork("fetched", blobOf('b'), "fetched")
	s.book("looked", "book", "application/pdf", blobOf('7'))
	if err := s.cat.RecordCoverAttempt(s.ctx, blobOf('7'), "pdf", "no page", now); err != nil {
		t.Fatal(err)
	}
	s.book("queued", "book", "application/pdf", blobOf('8'))
	s.exec(`INSERT INTO jobs (id, type, payload, state, dedupe_key, run_after, created_at, updated_at)
	        VALUES ('j1', ?, '{}', 'pending', ?, ?, ?, ?)`, cover.JobType, cover.DedupeKey(blobOf('8')), s.stamp, s.stamp, s.stamp)

	got := dueIDs(t, s)
	// EPUBs first (they need no capability), then PDFs by id. A fetched cover
	// does not exempt a book; a shipped one, an earlier look and a live job do.
	want := []string{"a-epub", "a-fetched", "a-pdf"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("due = %v, want %v", got, want)
	}
}

func TestRecordExtractedCoverLandsAsPreferredArtworkAndIsNotDueAgain(t *testing.T) {
	t.Parallel()
	s := newBookSeed(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	assetID := s.book("b", "book", "application/epub+zip", blobOf('1'))

	for range 2 { // idempotent across a re-run
		if err := s.cat.RecordExtractedCover(s.ctx, assetID, "epub", catalog.FetchedArtwork{
			BlobHash: blobOf('c'), Size: 42, MIME: "image/png", Source: "epub",
		}, now); err != nil {
			t.Fatal(err)
		}
	}

	var n int
	var edition, mime, ident string
	if err := s.db.Reader().QueryRowContext(s.ctx, `
		SELECT count(*), min(edition_id), min(mime), min(identification_source)
		FROM assets WHERE role = 'artwork'`).Scan(&n, &edition, &mime, &ident); err != nil {
		t.Fatal(err)
	}
	if n != 1 || edition != "e-b" || mime != "image/png" || ident != "extracted" {
		t.Fatalf("artwork rows = %d on %q (%s, %s), want one on e-b, image/png, extracted", n, edition, mime, ident)
	}

	var outcome string
	if err := s.db.Reader().QueryRowContext(s.ctx,
		`SELECT outcome FROM cover_extractions WHERE blob_hash = ?`, blobOf('1')).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != catalog.CoverOutcomeExtracted {
		t.Errorf("outcome = %q, want extracted", outcome)
	}
	if got := dueIDs(t, s); len(got) != 0 {
		t.Errorf("due after extraction = %v, want none", got)
	}
}

func TestCoverDedupePrefixMatchesTheJob(t *testing.T) {
	t.Parallel()
	// DueCoverExtractions reads the job table by dedupe key; if the prefix drifts
	// from cover.DedupeKey, every queued PDF would hold a batch slot forever.
	s := newBookSeed(t)
	s.book("q", "book", "application/pdf", blobOf('1'))
	s.exec(`INSERT INTO jobs (id, type, payload, state, dedupe_key, run_after, created_at, updated_at)
	        VALUES ('j1', ?, '{}', 'pending', ?, ?, ?, ?)`, cover.JobType, cover.DedupeKey(blobOf('1')), s.stamp, s.stamp, s.stamp)
	if got := dueIDs(t, s); len(got) != 0 {
		t.Fatalf("a book with a live job is due: %v", got)
	}
}
