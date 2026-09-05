// Search as the browse layer sees it (ADR-0075): works carry their attributes
// and poster, and the parts of a work that matched — scanned episodes and
// followed-source items — come back as episode hits.
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
	seriesWorkID    = "01990000-0000-7000-8000-0000000000w8"
	seriesEditionID = "01990000-0000-7000-8000-0000000000e8"
	episodeAssetID  = "01990000-0000-7000-8000-0000000000a9"
	episodeBlobHash = "blake3:8888888888888888888888888888888888888888888888888888888888888888"
	itemID          = "01990000-0000-7000-8000-0000000000i8"
)

// seedSeries adds a series with one scanned episode (an edition with a file)
// and one projected item (no file of its own), both titled so "pilot" finds
// them and nothing else does.
func (h *harness) seedSeries() *harness {
	h.t.Helper()
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES (?, 'series', 'a-series|2020', 'A Series', 'series', 2020, '{}', '2026-08-06T00:00:00Z', ?)`, seriesWorkID, seedTime)
	h.exec(`INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at)
		VALUES (?, ?, 'S01E01', 'web', 'en', '{"season":1,"episode":1,"episode_title":"Pilot Light"}', ?)`,
		seriesEditionID, seriesWorkID, seedTime)
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1073741824, 'video/mp4', ?)`, episodeBlobHash, seedTime)
	h.exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, source_path,
			role, filename, mime, identification_source, missing_since, created_at, updated_at) VALUES
		(?, ?, ?, 'managed', ?, '/srv/tv/s01e01.mp4', 'primary', 's01e01.mp4', 'video/mp4', 'path', NULL, ?, ?)`,
		episodeAssetID, seriesEditionID, libFilmsID, episodeBlobHash, seedTime, seedTime)
	h.exec(`INSERT INTO items (id, work_id, edition_id, item_key, title, published_at, attributes, created_at, updated_at)
		VALUES (?, ?, NULL, 'S01E02', 'Pilot Returns', '2026-08-07T00:00:00Z', '{"season":"1","episode":"2"}', ?, ?)`,
		itemID, seriesWorkID, seedTime, seedTime)
	return h
}

type searchOut struct {
	Works []struct {
		WorkID     string          `json:"work_id"`
		Attributes json.RawMessage `json:"attributes"`
		Artwork    json.RawMessage `json:"artwork"`
	} `json:"works"`
	Episodes []struct {
		Kind         string          `json:"kind"`
		ID           string          `json:"id"`
		WorkID       string          `json:"work_id"`
		Season       *int64          `json:"season"`
		Episode      *int64          `json:"episode"`
		PrimaryAsset json.RawMessage `json:"primary_asset"`
	} `json:"episodes"`
}

func (h *harness) search(t *testing.T, body string) (*http.Response, searchOut) {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/v1/search", "", strings.NewReader(body))
	var out searchOut
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(h.body(resp), &out); err != nil {
			t.Fatal(err)
		}
	}
	return resp, out
}

func TestSearchShapes(t *testing.T) {
	h := newHarness(t).seed().seedBrowse().seedSeries()
	tests := []struct {
		name, body, golden string
	}{
		{"a work with its poster", `{"query":"arrival"}`, "search_work.json"},
		{"episodes from an edition and an item", `{"query":"pilot"}`, "search_episodes.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.doStable(http.MethodPost, "/api/v1/search", strings.NewReader(tt.body))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
			}
			testutil.Golden(t, goldenPath(tt.golden), h.indent(resp))
		})
	}
}

func TestSearchFindsEpisodesByTheirOwnTitle(t *testing.T) {
	h := newHarness(t).seed().seedBrowse().seedSeries()

	_, out := h.search(t, `{"query":"pilot"}`)
	if len(out.Works) != 0 {
		t.Fatalf("no WORK is called pilot, got %+v", out.Works)
	}
	if len(out.Episodes) != 2 {
		t.Fatalf("episodes = %+v, want the scanned edition and the projected item", out.Episodes)
	}
	edition, item := out.Episodes[0], out.Episodes[1]
	if edition.Kind != "edition" || edition.ID != seriesEditionID || *edition.Season != 1 || *edition.Episode != 1 {
		t.Errorf("edition hit = %+v", edition)
	}
	if !strings.Contains(string(edition.PrimaryAsset), episodeAssetID) {
		t.Errorf("the edition hit should carry its file: %s", edition.PrimaryAsset)
	}
	if item.Kind != "item" || item.ID != itemID || *item.Season != 1 || *item.Episode != 2 {
		t.Errorf("item hit = %+v", item)
	}
	if len(item.PrimaryAsset) != 0 {
		t.Errorf("an item has no file of its own: %s", item.PrimaryAsset)
	}
	if edition.WorkID != seriesWorkID || item.WorkID != seriesWorkID {
		t.Error("both hits name the series they belong to")
	}
}

func TestSearchWorksCarryPosterAndAttributes(t *testing.T) {
	h := newHarness(t).seed().seedBrowse().seedSeries()

	_, out := h.search(t, `{"query":"arrival"}`)
	if len(out.Works) != 1 || len(out.Episodes) != 0 {
		t.Fatalf("got %d works, %d episodes", len(out.Works), len(out.Episodes))
	}
	if !strings.Contains(string(out.Works[0].Artwork), posterAssetID) {
		t.Errorf("artwork = %s, want the poster", out.Works[0].Artwork)
	}
	if !strings.Contains(string(out.Works[0].Attributes), "Villeneuve") {
		t.Errorf("attributes = %s", out.Works[0].Attributes)
	}

	// A work with no poster says null, and episodes is [] never null.
	resp := h.do(http.MethodPost, "/api/v1/search", "", strings.NewReader(`{"query":"blade"}`))
	body := string(h.body(resp))
	if !strings.Contains(body, `"artwork":null`) || !strings.Contains(body, `"episodes":[]`) {
		t.Fatalf("body = %s", body)
	}
}

func TestSearchContentTypeNarrowsBothLists(t *testing.T) {
	h := newHarness(t).seed().seedBrowse().seedSeries()
	_, out := h.search(t, `{"query":"pilot","content_type":"movie"}`)
	if len(out.Episodes) != 0 {
		t.Fatalf("a movie-only search returned series episodes: %+v", out.Episodes)
	}
	// A content_type-only search lists works and looks for no episodes.
	_, out = h.search(t, `{"content_type":"series"}`)
	if len(out.Works) != 1 || len(out.Episodes) != 0 {
		t.Fatalf("got %d works, %d episodes", len(out.Works), len(out.Episodes))
	}
}

func TestGuestSearchHidesAVaultPosterAndFile(t *testing.T) {
	h := newHarness(t, withAuth, withGuest).seed().seedSeries()
	seedVaultArtwork(h)
	// Make the episode's only file a vault one.
	h.exec(`UPDATE assets SET source_class = 'vault' WHERE id = ?`, episodeAssetID)

	guest := string(h.body(h.do(http.MethodPost, "/api/v1/search", "", strings.NewReader(`{"query":"blade"}`))))
	if strings.Contains(guest, vaultArtworkID) {
		t.Fatalf("guest search leaked a vault poster: %s", guest)
	}
	guest = string(h.body(h.do(http.MethodPost, "/api/v1/search", "", strings.NewReader(`{"query":"pilot"}`))))
	if strings.Contains(guest, episodeAssetID) {
		t.Fatalf("guest search leaked a vault file: %s", guest)
	}

	tok := h.mint("reader", auth.ScopeRead)
	reader := string(h.body(h.do(http.MethodPost, "/api/v1/search", tok.Secret, strings.NewReader(`{"query":"blade"}`))))
	if !strings.Contains(reader, vaultArtworkID) {
		t.Fatalf("read-token search hid a vault poster: %s", reader)
	}
}
