// The browse projections (ADR-0075): a work's poster and playable file as
// embeds on the listing and the detail read, the recent-first order under its
// own cursor, the year and grouping filters, and the artwork redirect.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// The browse fixture on top of seed(): a poster AND a fanart on the first
// work (so the picker's preference is tested, not just its presence), a probe
// on its blob (so duration is real), a music library with three albums by two
// artists, and a second book with an author — each created at a distinct
// instant so the recent-first order has something to order.
const (
	libMusicID = "01990000-0000-7000-8000-0000000000l3"
	work4ID    = "01990000-0000-7000-8000-0000000000w4"
	work5ID    = "01990000-0000-7000-8000-0000000000w5"
	work6ID    = "01990000-0000-7000-8000-0000000000w6"
	work7ID    = "01990000-0000-7000-8000-0000000000w7"
	edition4ID = "01990000-0000-7000-8000-0000000000e4"
	edition5ID = "01990000-0000-7000-8000-0000000000e5"
	edition6ID = "01990000-0000-7000-8000-0000000000e6"
	// The fanart's id sorts BEFORE the poster's, so an id-ordered pick would
	// choose the fanart; the rank must win.
	fanartAssetID = "01990000-0000-7000-8000-0000000000a4"
	posterAssetID = "01990000-0000-7000-8000-0000000000a5"
	track1AssetID = "01990000-0000-7000-8000-0000000000a6"
	track2AssetID = "01990000-0000-7000-8000-0000000000a7"
	track3AssetID = "01990000-0000-7000-8000-0000000000a8"

	posterBlobHash = "blake3:3333333333333333333333333333333333333333333333333333333333333333"
	fanartBlobHash = "blake3:4444444444444444444444444444444444444444444444444444444444444444"
	track1BlobHash = "blake3:5555555555555555555555555555555555555555555555555555555555555555"
	track2BlobHash = "blake3:6666666666666666666666666666666666666666666666666666666666666666"
	track3BlobHash = "blake3:7777777777777777777777777777777777777777777777777777777777777777"
)

func (h *harness) seedBrowse() *harness {
	h.t.Helper()

	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES
		(?, 204800, 'image/jpeg', ?), (?, 409600, 'image/jpeg', ?),
		(?, 31457280, 'audio/flac', ?), (?, 29360128, 'audio/flac', ?), (?, 27262976, 'audio/flac', ?)`,
		posterBlobHash, seedTime, fanartBlobHash, seedTime,
		track1BlobHash, seedTime, track2BlobHash, seedTime, track3BlobHash, seedTime)

	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at) VALUES
		(?, ?, ?, 'managed', ?, '/srv/films/fanart.jpg', 'artwork', 'fanart.jpg', 'image/jpeg', 'path', NULL, ?, ?),
		(?, ?, ?, 'managed', ?, '/srv/films/poster.jpg', 'artwork', 'poster.jpg', 'image/jpeg', 'path', NULL, ?, ?)`,
		fanartAssetID, edition1ID, libFilmsID, fanartBlobHash, seedTime, seedTime,
		posterAssetID, edition1ID, libFilmsID, posterBlobHash, seedTime, seedTime)

	h.exec(`INSERT INTO blob_probes (blob_hash, container, format_long, duration_seconds, bitrate_bps,
			streams, bytes_read, materialised, probed_at) VALUES
		(?, 'matroska,webm', 'Matroska / WebM', 6960.5, 49000000, '[]', 0, 0, ?)`, blob1Hash, seedTime)

	h.exec(`INSERT INTO libraries (id, name, content_type, enabled, created_at) VALUES (?, 'music', 'music', 1, ?)`,
		libMusicID, seedTime)
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at) VALUES
		(?, 'music', 'artist-a|album-one', 'Album One', 'album one', 2001, '{"artist":"Artist A"}', '2026-08-02T00:00:00Z', ?),
		(?, 'music', 'artist-a|album-two', 'Album Two', 'album two', 2004, '{"artist":"Artist A"}', '2026-08-03T00:00:00Z', ?),
		(?, 'music', 'artist-b|album-three', 'Album Three', 'album three', 2010, '{"artist":"Artist B"}', '2026-08-04T00:00:00Z', ?),
		(?, 'book', 'a-second-book|1999', 'A Second Book', 'second book', 1999, '{"author":"Author A"}', '2026-08-05T00:00:00Z', ?)`,
		work4ID, seedTime, work5ID, seedTime, work6ID, seedTime, work7ID, seedTime)
	h.exec(`INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at) VALUES
		(?, ?, 'flac', 'lossless', NULL, '{"track":1,"track_title":"One"}', ?),
		(?, ?, 'flac', 'lossless', NULL, '{"track":1,"track_title":"Two"}', ?),
		(?, ?, 'flac', 'lossless', NULL, '{"track":1,"track_title":"Three"}', ?)`,
		edition4ID, work4ID, seedTime, edition5ID, work5ID, seedTime, edition6ID, work6ID, seedTime)
	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at) VALUES
		(?, ?, ?, 'managed', ?, '/srv/music/one.flac', 'primary', 'one.flac', 'audio/flac', 'path', NULL, ?, ?),
		(?, ?, ?, 'managed', ?, '/srv/music/two.flac', 'primary', 'two.flac', 'audio/flac', 'path', NULL, ?, ?),
		(?, ?, ?, 'managed', ?, '/srv/music/three.flac', 'primary', 'three.flac', 'audio/flac', 'path', NULL, ?, ?)`,
		track1AssetID, edition4ID, libMusicID, track1BlobHash, seedTime, seedTime,
		track2AssetID, edition5ID, libMusicID, track2BlobHash, seedTime, seedTime,
		track3AssetID, edition6ID, libMusicID, track3BlobHash, seedTime, seedTime)
	return h
}

// noFollow issues a request whose redirect is returned rather than followed:
// the assertion is about the 307 the node answers, not about what lies
// behind it (the harness mounts no byte route).
func (h *harness) noFollow(method, path string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.http.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestBrowseShapes(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()

	tests := []struct {
		name   string
		path   string
		golden string
	}{
		{"cards", "/api/v1/works?content_type=movie&include=artwork,primary_asset", "works_cards.json"},
		{"artwork only", "/api/v1/works?content_type=movie&include=artwork", "works_cards_artwork.json"},
		{"recent first", "/api/v1/works?sort=recent&limit=3", "works_recent.json"},
		{"one work, with its embeds", "/api/v1/works/" + work1ID, "work_card.json"},
		{"an artist's albums", "/api/v1/works?content_type=music&artist=Artist+A", "works_by_artist.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.doStable(http.MethodGet, tt.path, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, h.body(resp))
			}
			testutil.Golden(t, goldenPath(tt.golden), h.indent(resp))
		})
	}
}

// The listing without `include` is byte-identical to what it was before the
// embeds existed: a client that never asked pays nothing and sees nothing new.
func TestWorksWithoutIncludeCarryNoEmbeds(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()
	body := string(h.body(h.get("/api/v1/works?content_type=movie")))
	if strings.Contains(body, `"artwork"`) || strings.Contains(body, `"primary_asset"`) {
		t.Fatalf("a plain listing rendered an embed:\n%s", body)
	}
}

func TestIncludeDistinguishesNotAskedFromNone(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()
	var pg struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(h.body(h.get("/api/v1/works?content_type=movie&include=artwork")), &pg); err != nil {
		t.Fatal(err)
	}
	for _, item := range pg.Items {
		if _, ok := item["primary_asset"]; ok {
			t.Error("primary_asset rendered though only artwork was asked for")
		}
		art, ok := item["artwork"]
		if !ok {
			t.Fatal("artwork absent though it was asked for")
		}
		switch string(item["id"]) {
		case `"` + work1ID + `"`:
			if !strings.Contains(string(art), posterAssetID) {
				t.Errorf("work1 artwork = %s, want the poster (rank beats id order)", art)
			}
		case `"` + work2ID + `"`:
			if string(art) != "null" {
				t.Errorf("work2 artwork = %s, want an explicit null", art)
			}
		}
	}
}

func TestIncludeRefusesAnUnknownEmbed(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()
	if resp := h.get("/api/v1/works?include=artwrok"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestYearFilters(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()
	tests := []struct {
		query string
		want  []string
	}{
		{"year=2016", []string{work1ID}},
		{"year_from=2017&year_to=2017", []string{work2ID}},
		{"content_type=music&year_from=2004", []string{work6ID, work5ID}}, // title order: "three" < "two"
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			ids := workIDs(t, h.body(h.get("/api/v1/works?"+tt.query)))
			if strings.Join(ids, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("ids = %v, want %v", ids, tt.want)
			}
		})
	}
	if resp := h.get("/api/v1/works?year=2O16"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a non-integer year = %d, want 400", resp.StatusCode)
	}
}

func TestAuthorFilter(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()
	ids := workIDs(t, h.body(h.get("/api/v1/works?content_type=book&author=Author+A")))
	if len(ids) != 1 || ids[0] != work7ID {
		t.Fatalf("ids = %v, want only the authored book", ids)
	}
}

// Recent-first pages under its own cursor. A title-order cursor is refused on
// it and vice versa: the two are different positions in different orders.
func TestRecentOrderPagesAndRefusesAForeignCursor(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()

	var first struct {
		Items      []struct{ ID string }
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(h.body(h.get("/api/v1/works?sort=recent&limit=2")), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.Items[0].ID != work7ID || first.Items[1].ID != work6ID {
		t.Fatalf("first page = %+v, want the two newest works", first.Items)
	}
	if first.NextCursor == "" {
		t.Fatal("no cursor on a page that has more")
	}

	// A work created between pages, newer than everything, must NOT surface on
	// page two: the position is fixed, so it belongs before page one.
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('01990000-0000-7000-8000-0000000000w9', 'movie', 'newest|2026', 'Newest', 'newest', 2026, '{}',
		'2026-08-09T00:00:00Z', ?)`, seedTime)

	second := workIDs(t, h.body(h.get("/api/v1/works?sort=recent&limit=2&cursor="+first.NextCursor)))
	if strings.Join(second, ",") != work5ID+","+work4ID {
		t.Fatalf("second page = %v, want the next two by creation", second)
	}

	if resp := h.get("/api/v1/works?sort=title&cursor=" + first.NextCursor); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a recent cursor on the title order = %d, want 400", resp.StatusCode)
	}
	if resp := h.get("/api/v1/works?sort=recent&cursor=" + worksCursor(t, h)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a title cursor on the recent order = %d, want 400", resp.StatusCode)
	}
	if resp := h.get("/api/v1/works?sort=newest"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown sort = %d, want 400", resp.StatusCode)
	}
}

// The embeds page correctly under the recent order too: the cursor is keyed on
// the stored created_at, not on a re-rendered time.
func TestRecentOrderWithEmbedsPages(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()
	var first struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(h.body(h.get("/api/v1/works?sort=recent&limit=2&include=artwork")), &first); err != nil {
		t.Fatal(err)
	}
	second := workIDs(t, h.body(h.get("/api/v1/works?sort=recent&limit=2&include=artwork&cursor="+first.NextCursor)))
	if strings.Join(second, ",") != work5ID+","+work4ID {
		t.Fatalf("second page = %v, want the next two by creation", second)
	}
}

func TestArtworkRedirectsToTheBlob(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()

	resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/artwork")
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", resp.StatusCode)
	}
	want := "/api/v1/blobs/" + posterBlobHash + "/content"
	if got := resp.Header.Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q (the poster, not the fanart)", got, want)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Errorf("Cache-Control = %q, want a private, short-lived policy on the work-keyed URL", cc)
	}

	// HEAD stays HEAD: that is what 307 buys over 302.
	if resp := h.noFollow(http.MethodHead, "/api/v1/works/"+work1ID+"/artwork"); resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("HEAD status = %d, want 307", resp.StatusCode)
	}
}

func TestArtworkIsNotFoundWhenAbsent(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()

	// An unknown work and a work with no poster are both 404s, and they say
	// different things.
	resp := h.noFollow(http.MethodGet, "/api/v1/works/nope/artwork")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown work = %d, want 404", resp.StatusCode)
	}
	resp = h.noFollow(http.MethodGet, "/api/v1/works/"+work3ID+"/artwork")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no artwork = %d, want 404", resp.StatusCode)
	}
	if body := string(h.body(resp)); !strings.Contains(body, "has no artwork") {
		t.Fatalf("the no-artwork 404 should say so: %s", body)
	}

	// An artwork that has gone missing from disk is not a poster a client can
	// fetch, so it is not picked.
	h.exec(`UPDATE assets SET missing_since = ? WHERE id IN (?, ?)`, seedTime, posterAssetID, fanartAssetID)
	if resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/artwork"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing artwork = %d, want 404", resp.StatusCode)
	}
}

func workIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var pg struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &pg); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	out := make([]string, 0, len(pg.Items))
	for _, it := range pg.Items {
		out = append(out, it.ID)
	}
	return out
}

// The recent order stays exact under writes, embeds included: walking every page
// while newer rows keep landing repeats nothing and skips nothing that existed
// when the walk began — the same guarantee the title order's stability test makes,
// on the order a shelf actually uses.
func TestRecentOrderWithEmbedsIsStableWhileTheTableIsWrittenTo(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()

	before := workIDs(t, h.body(h.get("/api/v1/works?sort=recent&limit=200")))

	var walked []string
	cursor := ""
	page := 0
	for {
		path := "/api/v1/works?sort=recent&limit=2&include=artwork,primary_asset"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var pg struct {
			Items []struct {
				ID      string          `json:"id"`
				Artwork json.RawMessage `json:"artwork"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(h.body(h.get(path)), &pg); err != nil {
			t.Fatal(err)
		}
		for _, it := range pg.Items {
			if len(it.Artwork) == 0 {
				t.Fatalf("page %d: the embed vanished on %s", page, it.ID)
			}
			walked = append(walked, it.ID)
		}
		// A newer work lands between every page.
		page++
		h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
			VALUES (?, 'movie', ?, 'Newer', 'newer', 2026, '{}', ?, ?)`,
			"01990000-0000-7000-8000-0000000000n"+string(rune('0'+page)), "newer|"+string(rune('0'+page)),
			"2026-09-0"+string(rune('0'+page))+"T00:00:00Z", seedTime)
		if pg.NextCursor == "" {
			break
		}
		cursor = pg.NextCursor
	}

	seen := map[string]int{}
	for _, id := range walked {
		seen[id]++
		if seen[id] > 1 {
			t.Fatalf("%s was returned twice", id)
		}
	}
	for _, id := range before {
		if seen[id] == 0 {
			t.Fatalf("%s existed before the walk and was never returned", id)
		}
	}
}

// ADR-0105: a cover the book file carried beats one fetched from an enrich
// source, even when both are named cover.jpg and the fetched one sorts first by
// id — the file's own cover is the exact one.
func TestAnExtractedCoverOutranksAFetchedOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()
	const (
		fetchedID   = "01990000-0000-7000-8000-0000000000c1"
		extractedID = "01990000-0000-7000-8000-0000000000c2"
		fetchedBlob = "blake3:8888888888888888888888888888888888888888888888888888888888888888"
		ownBlob     = "blake3:9999999999999999999999999999999999999999999999999999999999999999"
	)
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1, 'image/jpeg', ?), (?, 1, 'image/jpeg', ?)`,
		fetchedBlob, seedTime, ownBlob, seedTime)
	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at) VALUES
		(?, ?, ?, 'managed', ?, NULL, 'artwork', 'cover.jpg', 'image/jpeg', 'fetched', NULL, ?, ?),
		(?, ?, ?, 'managed', ?, NULL, 'artwork', 'cover.jpg', 'image/jpeg', 'extracted', NULL, ?, ?)`,
		fetchedID, edition1ID, libFilmsID, fetchedBlob, seedTime, seedTime,
		extractedID, edition1ID, libFilmsID, ownBlob, seedTime, seedTime)

	resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/artwork")
	want := "/api/v1/blobs/" + ownBlob + "/content"
	if got := resp.Header.Get("Location"); got != want {
		t.Fatalf("Location = %q, want the extracted cover %q", got, want)
	}
}

// Thumbnail redirect tests (ADR-0109) ──────────────────────────────────────

const thumbBlobHash = "blake3:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// seedThumbnail adds the thumb blob row and one ready derived_thumbnails row
// for (posterBlobHash, 256, recipe 1) inside the test harness.
func (h *harness) seedThumbnail() *harness {
	h.t.Helper()
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 8192, 'image/jpeg', ?)`,
		thumbBlobHash, seedTime)
	h.exec(`INSERT INTO derived_thumbnails (source_hash, bucket, recipe, status, thumb_hash, created_at)
		VALUES (?, 256, 1, 'ready', ?, ?)`, posterBlobHash, thumbBlobHash, seedTime)
	return h
}

// TestGetWorkArtworkSizeReady: a ready derived row redirects to the thumb blob
// with a cacheable Cache-Control policy.
func TestGetWorkArtworkSizeReady(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse().seedThumbnail()

	resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/artwork?size=256")
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", resp.StatusCode)
	}
	wantLoc := "/api/v1/blobs/" + thumbBlobHash + "/content"
	if got := resp.Header.Get("Location"); got != wantLoc {
		t.Errorf("Location = %q, want %q (thumb blob, not source)", got, wantLoc)
	}
	cc := resp.Header.Get("Cache-Control")
	if !strings.Contains(cc, "private") || !strings.Contains(cc, "max-age=300") {
		t.Errorf("Cache-Control = %q, want private, max-age=300", cc)
	}
}

// TestGetWorkArtworkSizePending: no derived row → enqueue job, redirect to
// the original blob with no-store so the client retries when ready.
func TestGetWorkArtworkSizePending(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse() // no seedThumbnail

	resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/artwork?size=512")
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", resp.StatusCode)
	}
	wantLoc := "/api/v1/blobs/" + posterBlobHash + "/content"
	if got := resp.Header.Get("Location"); got != wantLoc {
		t.Errorf("Location = %q, want original %q while pending", got, wantLoc)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store while pending", cc)
	}

	// The derivation job must have been enqueued.
	n := h.countRows(t, `SELECT COUNT(*) FROM jobs WHERE type = ? AND dedupe_key = ?`,
		"derive_thumbnail", "derive-thumbnail:"+posterBlobHash+":512")
	if n != 1 {
		t.Errorf("job count = %d, want 1 (idempotent enqueue on pending)", n)
	}
}

// TestGetWorkArtworkSizeUndecodable: undecodable row → fall back to the original
// blob with a cacheable response so the client stops retrying.
func TestGetWorkArtworkSizeUndecodable(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()
	h.exec(`INSERT INTO derived_thumbnails (source_hash, bucket, recipe, status, thumb_hash, created_at)
		VALUES (?, 1024, 1, 'undecodable', NULL, ?)`, posterBlobHash, seedTime)

	resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/artwork?size=1024")
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", resp.StatusCode)
	}
	wantLoc := "/api/v1/blobs/" + posterBlobHash + "/content"
	if got := resp.Header.Get("Location"); got != wantLoc {
		t.Errorf("Location = %q, want original %q for undecodable", got, wantLoc)
	}
	cc := resp.Header.Get("Cache-Control")
	if !strings.Contains(cc, "private") || !strings.Contains(cc, "max-age=300") {
		t.Errorf("Cache-Control = %q, want private, max-age=300 for undecodable", cc)
	}
}

// TestGetWorkArtworkSizeInvalid: an unsupported size returns 400.
func TestGetWorkArtworkSizeInvalid(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()

	resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/artwork?size=999")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unsupported size", resp.StatusCode)
	}
}

// TestArtworkSizesInEmbed: when ?include=artwork is present the embed's sizes
// map contains all three buckets, and each URL carries both the ?size= and the
// ?v= cache-buster set to the source blob hash.
func TestArtworkSizesInEmbed(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()

	var card struct {
		Artwork *struct {
			Sizes map[string]string `json:"sizes"`
		} `json:"artwork"`
	}
	body := h.body(h.get("/api/v1/works/" + work1ID + "?include=artwork"))
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if card.Artwork == nil {
		t.Fatal("artwork embed is nil")
	}
	for _, b := range []string{"256", "512", "1024"} {
		url, ok := card.Artwork.Sizes[b]
		if !ok {
			t.Errorf("sizes[%q] missing", b)
			continue
		}
		wantSize := "size=" + b
		wantV := "v=" + posterBlobHash
		if !strings.Contains(url, wantSize) {
			t.Errorf("sizes[%q] = %q, want ?size=%s", b, url, b)
		}
		if !strings.Contains(url, wantV) {
			t.Errorf("sizes[%q] = %q, want &v=%s", b, url, posterBlobHash)
		}
	}
}
