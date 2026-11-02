// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
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

// The backdrop projection (ADR-0122): a work's wide image, resolved SEPARATELY
// from its poster. seedBrowse gives work1 a shipped fanart.jpg (the backdrop)
// and a poster.jpg (the poster), with the fanart's id sorting first so an
// unranked pick would confuse the two.

type faceRef struct {
	AssetID  string            `json:"asset_id"`
	BlobHash string            `json:"blob_hash"`
	Sizes    map[string]string `json:"sizes"`
}

type faces struct {
	ID       string          `json:"id"`
	Artwork  json.RawMessage `json:"artwork"`
	Backdrop json.RawMessage `json:"backdrop"`
}

func decodeFace(t *testing.T, raw json.RawMessage) *faceRef {
	t.Helper()
	if string(raw) == "null" {
		return nil
	}
	var f faceRef
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decoding a face: %v (%s)", err, raw)
	}
	return &f
}

func TestBackdropEmbedIsResolvedBesideThePoster(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()

	// The card listing, asked for both faces.
	var pg struct{ Items []faces }
	if err := json.Unmarshal(h.body(h.get("/api/v1/works?content_type=movie&include=artwork,backdrop")), &pg); err != nil {
		t.Fatal(err)
	}
	for _, it := range pg.Items {
		switch it.ID {
		case work1ID:
			art, bd := decodeFace(t, it.Artwork), decodeFace(t, it.Backdrop)
			if art == nil || art.AssetID != posterAssetID {
				t.Errorf("work1 artwork = %s, want the poster", it.Artwork)
			}
			if bd == nil || bd.AssetID != fanartAssetID {
				t.Errorf("work1 backdrop = %s, want the fanart", it.Backdrop)
			}
			if bd != nil && !strings.HasPrefix(bd.Sizes["256"], "/api/v1/works/"+work1ID+"/backdrop?size=256&v=") {
				t.Errorf("backdrop sizes must point at the backdrop route, got %v", bd.Sizes)
			}
		case work2ID:
			if string(it.Artwork) != "null" || string(it.Backdrop) != "null" {
				t.Errorf("work2 has no images: artwork=%s backdrop=%s", it.Artwork, it.Backdrop)
			}
		}
	}

	// Asked for only the poster, the backdrop key is ABSENT, not null.
	body := string(h.body(h.get("/api/v1/works?content_type=movie&include=artwork")))
	if strings.Contains(body, `"backdrop"`) {
		t.Error("backdrop rendered though it was not asked for")
	}

	// The detail read carries both, always.
	var detail faces
	if err := json.Unmarshal(h.body(h.get("/api/v1/works/"+work1ID)), &detail); err != nil {
		t.Fatal(err)
	}
	if art := decodeFace(t, detail.Artwork); art == nil || art.AssetID != posterAssetID {
		t.Errorf("detail artwork = %s", detail.Artwork)
	}
	if bd := decodeFace(t, detail.Backdrop); bd == nil || bd.AssetID != fanartAssetID {
		t.Errorf("detail backdrop = %s", detail.Backdrop)
	}

	// The golden shape of a card with the backdrop embed.
	resp := h.get("/api/v1/works?content_type=movie&include=backdrop")
	testutil.Golden(t, goldenPath("works_cards_backdrop.json"), h.indent(resp))
}

func TestBackdropRedirectsToItsOwnBlob(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()

	resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/backdrop")
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("backdrop = %d, want 307", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/api/v1/blobs/"+fanartBlobHash+"/content" {
		t.Fatalf("Location = %q, want the fanart's blob", loc)
	}
	if resp := h.noFollow(http.MethodHead, "/api/v1/works/"+work1ID+"/backdrop"); resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("HEAD backdrop = %d, want 307", resp.StatusCode)
	}
	// The thumbnail cache applies to a backdrop exactly as to a poster: the
	// first sized request redirects to the original with no-store while the
	// derivation is queued (ADR-0109).
	resp = h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/backdrop?size=512")
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("sized backdrop = %d %q, want a no-store 307 while the thumbnail derives", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/backdrop?size=300"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown bucket = %d, want 400", resp.StatusCode)
	}
	if resp := h.noFollow(http.MethodGet, "/api/v1/works/nope/backdrop"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown work = %d, want 404", resp.StatusCode)
	}
}

// The property this whole projection exists for: a poster is never served as a
// backdrop, and the client is told so rather than handed the poster again.
func TestBackdropIsNeverThePoster(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()

	// Only the poster remains.
	h.exec(`DELETE FROM assets WHERE id = ?`, fanartAssetID)
	resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/backdrop")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("backdrop with only a poster = %d, want 404", resp.StatusCode)
	}
	if body := string(h.body(resp)); !strings.Contains(body, "has no backdrop") {
		t.Fatalf("the no-backdrop 404 should say so: %s", body)
	}
	if resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/artwork"); resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("the poster is still served: %d", resp.StatusCode)
	}
	var detail faces
	if err := json.Unmarshal(h.body(h.get("/api/v1/works/"+work1ID)), &detail); err != nil {
		t.Fatal(err)
	}
	if string(detail.Backdrop) != "null" || decodeFace(t, detail.Artwork) == nil {
		t.Fatalf("detail = artwork %s, backdrop %s; want the poster and a null backdrop", detail.Artwork, detail.Backdrop)
	}

	// A banner is decoration but not a backdrop either.
	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at) VALUES
		(?, ?, ?, 'managed', ?, '/srv/films/banner.jpg', 'artwork', 'banner.jpg', 'image/jpeg', 'path', NULL, ?, ?)`,
		"01990000-0000-7000-8000-0000000000b1", edition1ID, libFilmsID, fanartBlobHash, seedTime, seedTime)
	if resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/backdrop"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a banner is not a backdrop: %d", resp.StatusCode)
	}
}

// A backdrop the enrich job fetched is filed as backdrop.jpg and lands in this
// slot — and ranks below one the release shipped (ADR-0105's rule), while the
// poster slot is untouched by it.
func TestFetchedBackdropRanksBelowAShippedOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()
	const fetchedBlob = "blake3:9999999999999999999999999999999999999999999999999999999999999999"
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 100, 'image/jpeg', ?)`, fetchedBlob, seedTime)
	// An id that sorts BEFORE the fanart's, so only the rank can put it second.
	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, attributes, missing_since, created_at, updated_at) VALUES
		(?, ?, ?, 'managed', ?, NULL, 'artwork', 'backdrop.jpg', 'image/jpeg', 'fetched', '{"source":"tmdb","kind":"backdrop"}', NULL, ?, ?)`,
		"01990000-0000-7000-8000-0000000000a0", edition1ID, libFilmsID, fetchedBlob, seedTime, seedTime)

	resp := h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/backdrop")
	if loc := resp.Header.Get("Location"); loc != "/api/v1/blobs/"+fanartBlobHash+"/content" {
		t.Fatalf("Location = %q, want the SHIPPED fanart ahead of the fetched backdrop", loc)
	}
	resp = h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/artwork")
	if loc := resp.Header.Get("Location"); loc != "/api/v1/blobs/"+posterBlobHash+"/content" {
		t.Fatalf("Location = %q, want the poster untouched by a fetched backdrop", loc)
	}

	// With the shipped fanart gone, the fetched backdrop is what serves.
	h.exec(`DELETE FROM assets WHERE id = ?`, fanartAssetID)
	resp = h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/backdrop")
	if loc := resp.Header.Get("Location"); loc != "/api/v1/blobs/"+fetchedBlob+"/content" {
		t.Fatalf("Location = %q, want the fetched backdrop", loc)
	}
}

// A series whose release shipped only per-episode `-thumb.jpg` stills showed one
// of those 16:9 frames as its card. The stills (and a logo) now rank below every
// other image: a fetched poster wins, and with no poster the fanart does — a still
// is a card only when it is all there is.
func TestEpisodeStillsNeverBeatARealCardImage(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedBrowse()
	const (
		stillBlob = "blake3:8888888888888888888888888888888888888888888888888888888888888888"
		logoBlob  = "blake3:8888888888888888888888888888888888888888888888888888888888888889"
		stillID   = "01990000-0000-7000-8000-0000000000c1"
		logoID    = "01990000-0000-7000-8000-0000000000c2"
	)
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 100, 'image/jpeg', ?), (?, 100, 'image/png', ?)`,
		stillBlob, seedTime, logoBlob, seedTime)
	// Ids that sort BEFORE every seeded asset, so only the rank can put them last.
	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at) VALUES
		(?, ?, ?, 'managed', ?, '/srv/media/show/S01E01-thumb.jpg', 'artwork', 'Show - S01E01 - Pilot-thumb.jpg', 'image/jpeg', 'path', NULL, ?, ?),
		(?, ?, ?, 'managed', ?, '/srv/media/show/clearlogo.png', 'artwork', 'clearlogo.png', 'image/png', 'path', NULL, ?, ?)`,
		stillID, edition1ID, libFilmsID, stillBlob, seedTime, seedTime,
		logoID, edition1ID, libFilmsID, logoBlob, seedTime, seedTime)

	location := func() string {
		return h.noFollow(http.MethodGet, "/api/v1/works/"+work1ID+"/artwork").Header.Get("Location")
	}
	if loc := location(); loc != "/api/v1/blobs/"+posterBlobHash+"/content" {
		t.Fatalf("with a poster, Location = %q, want the poster", loc)
	}
	// No poster: the fanart, not the still or the logo.
	h.exec(`DELETE FROM assets WHERE id = ?`, posterAssetID)
	if loc := location(); loc != "/api/v1/blobs/"+fanartBlobHash+"/content" {
		t.Fatalf("without a poster, Location = %q, want the fanart over the still and the logo", loc)
	}
	// Only the still and the logo left: the still is better than nothing.
	h.exec(`DELETE FROM assets WHERE id = ?`, fanartAssetID)
	if loc := location(); loc != "/api/v1/blobs/"+stillBlob+"/content" && loc != "/api/v1/blobs/"+logoBlob+"/content" {
		t.Fatalf("with only unfit art, Location = %q, want one of them rather than a 404", loc)
	}
	// A fetched poster beats the shipped still, though the still was shipped.
	const fetchedPoster = "blake3:8888888888888888888888888888888888888888888888888888888888888890"
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 100, 'image/jpeg', ?)`, fetchedPoster, seedTime)
	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at) VALUES
		(?, ?, ?, 'managed', ?, NULL, 'artwork', 'cover.jpg', 'image/jpeg', 'fetched', NULL, ?, ?)`,
		"01990000-0000-7000-8000-0000000000c3", edition1ID, libFilmsID, fetchedPoster, seedTime, seedTime)
	if loc := location(); loc != "/api/v1/blobs/"+fetchedPoster+"/content" {
		t.Fatalf("Location = %q, want the fetched poster over a shipped episode still", loc)
	}
}
