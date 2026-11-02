package catalog_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// Enrichment of held films and series (ADR-0122): the due predicate, the
// handling-time context and the two artwork kinds, against a real migrated
// database. The harness seeds one movie Work, w1 ("Arrival", 2016), with no
// edition — these tests hold it, decorate it piece by piece, and watch it leave
// the due set only when it has everything.

const videoStamp = "2026-08-01T00:00:00Z"

// holdWork gives a Work an edition with a managed primary asset: the "held"
// half of the due predicate.
func holdWork(t *testing.T, h *harness, workID, editionID, assetID, blob string) {
	t.Helper()
	// A fetched image gets a self-peer replica row, so the node needs to know
	// who it is. ON CONFLICT so a test may hold several Works.
	h.exec(t, `INSERT INTO peers (id, name, site, mode, is_self, created_at)
		VALUES ('self', 'peer-a', 'site-a', 'full', 1, ?) ON CONFLICT (id) DO NOTHING`, videoStamp)
	h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1000, 'video/x-matroska', ?)
		ON CONFLICT (hash) DO NOTHING`, blob, videoStamp)
	h.exec(t, `INSERT INTO editions (id, work_id, label, edition_type, attributes, created_at)
		VALUES (?, ?, 'main', '', '{}', ?)`, editionID, workID, videoStamp)
	h.exec(t, `INSERT INTO assets (id, edition_id, source_class, blob_hash, source_path, role, filename, mime,
			identification_source, created_at, updated_at)
		VALUES (?, ?, 'managed', ?, '/srv/media/file.mkv', 'primary', 'file.mkv', 'video/x-matroska', 'path', ?, ?)`,
		assetID, editionID, blob, videoStamp, videoStamp)
}

// shipArtwork records an artwork asset as a scan would — the release shipped
// this file.
func shipArtwork(t *testing.T, h *harness, editionID, assetID, blob, filename string) {
	t.Helper()
	h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 500, 'image/jpeg', ?)
		ON CONFLICT (hash) DO NOTHING`, blob, videoStamp)
	h.exec(t, `INSERT INTO assets (id, edition_id, source_class, blob_hash, source_path, role, filename, mime,
			identification_source, created_at, updated_at)
		VALUES (?, ?, 'managed', ?, ?, 'artwork', ?, 'image/jpeg', 'path', ?, ?)`,
		assetID, editionID, blob, "/srv/media/"+filename, filename, videoStamp, videoStamp)
}

func dueEnrichIDs(t *testing.T, h *harness) []string {
	t.Helper()
	due, err := h.cat.DueEnrichWorks(context.Background(), time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(due))
	for _, d := range due {
		ids = append(ids, d.WorkID)
	}
	return ids
}

func TestAHeldMovieIsDueUntilItHasIDPosterAndBackdrop(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	// Wanted but not held: nothing to decorate, not due.
	if ids := dueEnrichIDs(t, h); len(ids) != 0 {
		t.Fatalf("an unheld work is not enriched, got %v", ids)
	}

	holdWork(t, h, "w1", "e1", "a1", "blake3:aaaa000000000000000000000000000000000000000000000000000000000001")
	if ids := dueEnrichIDs(t, h); len(ids) != 1 || ids[0] != "w1" {
		t.Fatalf("a held movie with nothing is due, got %v", ids)
	}

	// A shipped poster alone: still due — no backdrop, no id.
	shipArtwork(t, h, "e1", "art-poster", "blake3:aaaa000000000000000000000000000000000000000000000000000000000002", "poster.jpg")
	if ids := dueEnrichIDs(t, h); len(ids) != 1 {
		t.Fatalf("a movie with only a poster is still due, got %v", ids)
	}
	wctx, ok, err := h.cat.EnrichContext(ctx, "w1")
	if err != nil || !ok {
		t.Fatalf("context: ok=%v err=%v", ok, err)
	}
	if !wctx.HasPoster || wctx.HasBackdrop {
		t.Fatalf("context = %+v, want a poster and no backdrop", wctx)
	}
	if wctx.ContentType != "movie" || wctx.Title != "Arrival" || wctx.Year != 2016 {
		t.Fatalf("context identity = %+v", wctx)
	}

	// The id: still due — no backdrop.
	if err := h.cat.WriteWorkExternalID(ctx, "w1", "tmdb", "329865"); err != nil {
		t.Fatal(err)
	}
	if ids := dueEnrichIDs(t, h); len(ids) != 1 {
		t.Fatalf("a movie with a poster and an id but no backdrop is still due, got %v", ids)
	}
	wctx, _, _ = h.cat.EnrichContext(ctx, "w1")
	if wctx.ExternalIDs["tmdb"] != "329865" {
		t.Fatalf("the context must carry the recorded ids as strong keys, got %v", wctx.ExternalIDs)
	}

	// A fetched backdrop completes it.
	if err := h.cat.RecordFetchedArtwork(ctx, "w1", catalog.FetchedArtwork{
		BlobHash: "blake3:aaaa000000000000000000000000000000000000000000000000000000000003",
		Size:     900, MIME: "image/jpeg", Source: "tmdb", Kind: catalog.ArtworkBackdrop,
	}, time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if ids := dueEnrichIDs(t, h); len(ids) != 0 {
		t.Fatalf("a movie with id, poster and backdrop is fully enriched, got %v", ids)
	}
	wctx, _, _ = h.cat.EnrichContext(ctx, "w1")
	if !wctx.HasPoster || !wctx.HasBackdrop {
		t.Fatalf("context = %+v, want both faces", wctx)
	}
}

// A music Work owes no backdrop: an album with a cover and its MusicBrainz id is
// done. The generalised predicate must not have made every album due forever.
func TestAnAlbumOwesNoBackdrop(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	h.exec(t, `UPDATE works SET content_type = 'music', attributes = '{"artist":"Artist A"}' WHERE id = 'w1'`)
	holdWork(t, h, "w1", "e1", "a1", "blake3:bbbb000000000000000000000000000000000000000000000000000000000001")
	shipArtwork(t, h, "e1", "art-cover", "blake3:bbbb000000000000000000000000000000000000000000000000000000000002", "cover.jpg")
	if err := h.cat.WriteWorkExternalID(ctx, "w1", "musicbrainz", "mbid-1"); err != nil {
		t.Fatal(err)
	}
	if ids := dueEnrichIDs(t, h); len(ids) != 0 {
		t.Fatalf("an album with a cover and an id is done, got %v", ids)
	}
	// And a TMDB id on an album is not the id that counts for it.
	h.exec(t, `DELETE FROM external_ids WHERE source = 'musicbrainz'`)
	if err := h.cat.WriteWorkExternalID(ctx, "w1", "tmdb", "1"); err != nil {
		t.Fatal(err)
	}
	if ids := dueEnrichIDs(t, h); len(ids) != 1 {
		t.Fatalf("an album identified only by a tmdb id is not identified, got %v", ids)
	}
}

// Decoration is not a poster: a film that shipped only fanart.jpg has a
// backdrop and no front image, and is due for its poster.
func TestFanartAloneIsNotAPoster(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	holdWork(t, h, "w1", "e1", "a1", "blake3:cccc000000000000000000000000000000000000000000000000000000000001")
	shipArtwork(t, h, "e1", "art-fanart", "blake3:cccc000000000000000000000000000000000000000000000000000000000002", "fanart.jpg")
	if err := h.cat.WriteWorkExternalID(ctx, "w1", "tmdb", "1"); err != nil {
		t.Fatal(err)
	}
	wctx, _, _ := h.cat.EnrichContext(ctx, "w1")
	if wctx.HasPoster || !wctx.HasBackdrop {
		t.Fatalf("context = %+v, want a backdrop and no poster", wctx)
	}
	if ids := dueEnrichIDs(t, h); len(ids) != 1 {
		t.Fatalf("fanart alone leaves the film due for a poster, got %v", ids)
	}
}

// The two kinds land under the filenames the resolver ranks on, and a re-run
// converges on the same rows rather than adding more.
func TestFetchedArtworkKindsAreFiledByName(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	holdWork(t, h, "w1", "e1", "a1", "blake3:dddd000000000000000000000000000000000000000000000000000000000001")
	now := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)
	poster := catalog.FetchedArtwork{
		BlobHash: "blake3:dddd000000000000000000000000000000000000000000000000000000000002",
		Size:     1, MIME: "image/jpeg", Source: "tmdb", Kind: catalog.ArtworkPoster,
	}
	backdrop := catalog.FetchedArtwork{
		BlobHash: "blake3:dddd000000000000000000000000000000000000000000000000000000000003",
		Size:     1, MIME: "image/jpeg", Source: "tmdb", Kind: catalog.ArtworkBackdrop,
	}
	for _, art := range []catalog.FetchedArtwork{poster, backdrop, poster, backdrop} {
		if err := h.cat.RecordFetchedArtwork(ctx, "w1", art, now); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := h.db.Reader().Query(`SELECT filename, blob_hash, json_extract(attributes, '$.kind')
		FROM assets WHERE role = 'artwork' ORDER BY filename`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got [][3]string
	for rows.Next() {
		var f, b, k string
		if err := rows.Scan(&f, &b, &k); err != nil {
			t.Fatal(err)
		}
		got = append(got, [3]string{f, b, k})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := [][3]string{
		{"backdrop.jpg", backdrop.BlobHash, "backdrop"},
		{"cover.jpg", poster.BlobHash, "poster"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("artwork rows = %v, want exactly %v (two kinds, no duplicates on re-run)", got, want)
	}
	// One asset.created per kind, not per attempt.
	evs, err := h.events.Since(ctx, 0, []string{events.TypeAssetCreated}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("asset.created events = %d, want 2", len(evs))
	}
}

func TestRecordWorkMetadataAddsAndPreservesForeignValues(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)

	meta := catalog.EnrichedMetadata{Source: "tmdb", Overview: "Aliens arrive.", Genres: []string{"Drama", "Science Fiction"}, Year: 2016}
	if err := h.cat.RecordWorkMetadata(ctx, "w1", meta, now); err != nil {
		t.Fatal(err)
	}
	attrs := workAttrs(t, h, "w1")
	if attrs["overview"] != "Aliens arrive." || attrs["enrich_source"] != "tmdb" || attrs["enriched_at"] != "2026-08-01T01:00:00Z" {
		t.Fatalf("attributes = %v", attrs)
	}
	if genres, _ := attrs["genres"].([]any); len(genres) != 2 || genres[0] != "Drama" {
		t.Fatalf("genres = %v", attrs["genres"])
	}
	if y, _ := attrs["enrich_year"].(float64); int(y) != 2016 {
		t.Fatalf("enrich_year = %v", attrs["enrich_year"])
	}
	var title string
	var year int
	if err := h.db.Reader().QueryRow(`SELECT title, year FROM works WHERE id = 'w1'`).Scan(&title, &year); err != nil {
		t.Fatal(err)
	}
	if title != "Arrival" || year != 2016 {
		t.Fatalf("identity changed to %q/%d — metadata is additive", title, year)
	}
	if updated := h.eventsOf(t, events.TypeWorkUpdated); updated != 1 {
		t.Fatalf("work.updated events = %d, want 1", updated)
	}

	// The same answer again changes nothing and emits nothing (invariant 9).
	if err := h.cat.RecordWorkMetadata(ctx, "w1", meta, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if h.eventsOf(t, events.TypeWorkUpdated) != 1 {
		t.Fatal("a no-op re-run must not emit")
	}
	if workAttrs(t, h, "w1")["enriched_at"] != "2026-08-01T01:00:00Z" {
		t.Fatal("a no-op re-run must not restamp")
	}

	// A refreshed overview from the same source is taken.
	meta.Overview = "Aliens arrive, and linguistics follows."
	if err := h.cat.RecordWorkMetadata(ctx, "w1", meta, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if workAttrs(t, h, "w1")["overview"] != meta.Overview {
		t.Fatal("the same source may refresh what it wrote")
	}

	// A value another writer set is left alone.
	h.exec(t, `UPDATE works SET attributes = json_set(attributes, '$.overview', 'A person wrote this.', '$.enrich_source', 'hand') WHERE id = 'w1'`)
	if err := h.cat.RecordWorkMetadata(ctx, "w1", meta, now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if workAttrs(t, h, "w1")["overview"] != "A person wrote this." {
		t.Fatal("an overview some other writer set must not be replaced by a provider's")
	}

	// Nothing to say is a no-op, not an error.
	if err := h.cat.RecordWorkMetadata(ctx, "w1", catalog.EnrichedMetadata{Source: "tmdb"}, now); err != nil {
		t.Fatal(err)
	}
}

func TestWorksNeedingEnrichScopesByContentType(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	holdWork(t, h, "w1", "e1", "a1", "blake3:eeee000000000000000000000000000000000000000000000000000000000001")
	h.exec(t, `INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w2', 'book', 'book:a-book', 'A Book', 'book', NULL, '{"author":"Someone"}', ?, ?)`, videoStamp, videoStamp)
	holdWork(t, h, "w2", "e2", "a2", "blake3:eeee000000000000000000000000000000000000000000000000000000000002")

	all, err := h.cat.WorksNeedingEnrich(ctx, catalog.EnrichScope{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all = %v, want both works", all)
	}
	movies, err := h.cat.WorksNeedingEnrich(ctx, catalog.EnrichScope{All: true, ContentType: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	if len(movies) != 1 || movies[0] != "w1" {
		t.Fatalf("movies = %v, want just w1", movies)
	}

	backlog, err := h.cat.EnrichBacklogCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.UnderEnriched != 2 || backlog.Total != 2 {
		t.Fatalf("backlog = %+v", backlog)
	}
	if backlog.ByContentType["movie"] != (catalog.EnrichBacklogCount{UnderEnriched: 1, Total: 1}) ||
		backlog.ByContentType["book"] != (catalog.EnrichBacklogCount{UnderEnriched: 1, Total: 1}) {
		t.Fatalf("by type = %+v", backlog.ByContentType)
	}
}

func workAttrs(t *testing.T, h *harness, id string) map[string]any {
	t.Helper()
	var raw string
	if err := h.db.Reader().QueryRow(`SELECT attributes FROM works WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		t.Fatal(err)
	}
	return attrs
}

func (h *harness) eventsOf(t *testing.T, typ string) int {
	t.Helper()
	evs, err := h.events.Since(context.Background(), 0, []string{typ}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}
