// Artists and authors as groupings over works (ADR-0075).
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

const (
	albumCoverAssetID = "01990000-0000-7000-8000-000000000a10"
	albumCoverBlob    = "blake3:9999999999999999999999999999999999999999999999999999999999999999"
)

// seedAlbumCover gives Artist A's FIRST album (by year) a cover, so the group
// picks it and not the later album's absence.
func seedAlbumCover(h *harness) {
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 65536, 'image/jpeg', ?)`, albumCoverBlob, seedTime)
	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at) VALUES
		(?, ?, ?, 'managed', ?, '/srv/music/cover.jpg', 'artwork', 'cover.jpg', 'image/jpeg', 'path', NULL, ?, ?)`,
		albumCoverAssetID, edition4ID, libMusicID, albumCoverBlob, seedTime, seedTime)
}

type groupsOut struct {
	Items []struct {
		Name      string          `json:"name"`
		WorkCount int64           `json:"work_count"`
		Artwork   json.RawMessage `json:"artwork"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func (h *harness) groups(t *testing.T, path string) groupsOut {
	t.Helper()
	resp := h.get(path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, h.body(resp))
	}
	var out groupsOut
	if err := json.Unmarshal(h.body(resp), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGroupingShapes(t *testing.T) {
	h := newHarness(t).seed().seedBrowse()
	seedAlbumCover(h)
	for _, tt := range []struct{ name, path, golden string }{
		{"artists", "/api/v1/artists", "artists_list.json"},
		{"authors", "/api/v1/authors", "authors_list.json"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.doStable(http.MethodGet, tt.path, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			testutil.Golden(t, goldenPath(tt.golden), h.indent(resp))
		})
	}
}

func TestArtistsGroupCountAndPickTheFirstCover(t *testing.T) {
	h := newHarness(t).seed().seedBrowse()
	seedAlbumCover(h)
	out := h.groups(t, "/api/v1/artists")
	if len(out.Items) != 2 || out.Items[0].Name != "Artist A" || out.Items[1].Name != "Artist B" {
		t.Fatalf("artists = %+v", out.Items)
	}
	if out.Items[0].WorkCount != 2 || out.Items[1].WorkCount != 1 {
		t.Errorf("counts = %d, %d", out.Items[0].WorkCount, out.Items[1].WorkCount)
	}
	if !strings.Contains(string(out.Items[0].Artwork), albumCoverAssetID) {
		t.Errorf("Artist A's picture should be the first album's cover: %s", out.Items[0].Artwork)
	}
	if string(out.Items[1].Artwork) != "null" {
		t.Errorf("Artist B has no cover: %s", out.Items[1].Artwork)
	}
}

func TestGroupingsPageAndFilter(t *testing.T) {
	h := newHarness(t).seed().seedBrowse()
	first := h.groups(t, "/api/v1/artists?limit=1")
	if len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	second := h.groups(t, "/api/v1/artists?limit=1&cursor="+first.NextCursor)
	if len(second.Items) != 1 || second.Items[0].Name != "Artist B" || second.NextCursor != "" {
		t.Fatalf("second page = %+v", second)
	}
	if resp := h.get("/api/v1/authors?cursor=" + first.NextCursor); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an artists cursor on authors = %d, want 400", resp.StatusCode)
	}
	if got := h.groups(t, "/api/v1/artists?q=%20b"); len(got.Items) != 1 || got.Items[0].Name != "Artist B" {
		t.Fatalf("q filter = %+v", got.Items)
	}
	if got := h.groups(t, "/api/v1/authors"); len(got.Items) != 1 || got.Items[0].Name != "Author A" {
		t.Fatalf("authors = %+v", got.Items)
	}
}

func TestGuestArtistPictureHidesAVaultCover(t *testing.T) {
	h := newHarness(t, withAuth, withGuest).seed().seedBrowse()
	seedAlbumCover(h)
	h.exec(`UPDATE assets SET source_class = 'vault' WHERE id = ?`, albumCoverAssetID)

	guest := string(h.body(h.get("/api/v1/artists")))
	if strings.Contains(guest, albumCoverAssetID) {
		t.Fatalf("guest artists leaked a vault cover: %s", guest)
	}
	tok := h.mint("reader", auth.ScopeRead)
	reader := string(h.body(h.do(http.MethodGet, "/api/v1/artists", tok.Secret, nil)))
	if !strings.Contains(reader, albumCoverAssetID) {
		t.Fatalf("read-token artists hid a cover: %s", reader)
	}
}
