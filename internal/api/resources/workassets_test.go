//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// GET /works/{id}/assets is the join a work-detail screen used to do by hand
// (#429). The response shape is a golden file; these are the assertions a
// golden cannot make.

type workAssetsPage struct {
	Items []struct {
		ID           string  `json:"id"`
		EditionID    string  `json:"edition_id"`
		EditionLabel string  `json:"edition_label"`
		EditionType  string  `json:"edition_type"`
		BlobHash     *string `json:"blob_hash"`
		BlobSize     *int64  `json:"blob_size"`
		BlobMIME     *string `json:"blob_mime"`
		MissingSince *string `json:"missing_since"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func (h *harness) workAssets(t *testing.T, path string) workAssetsPage {
	t.Helper()
	resp := h.get(path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
	}
	var page workAssetsPage
	if err := json.Unmarshal(h.body(resp), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

// The listing is scoped to ONE work, resolved through its editions, and carries
// the blob size the screen would otherwise fetch per asset.
func TestWorkAssetsAreScopedAndCarryBlobFacts(t *testing.T) {
	h := newHarness(t).seed()

	page := h.workAssets(t, "/api/v1/works/"+work1ID+"/assets")
	if len(page.Items) != 1 {
		t.Fatalf("got %d assets for work 1, want 1", len(page.Items))
	}
	got := page.Items[0]
	if got.ID != asset1ID || got.EditionID != edition1ID {
		t.Errorf("got asset %s of edition %s, want %s of %s",
			got.ID, got.EditionID, asset1ID, edition1ID)
	}
	if got.EditionLabel != "2160p" || got.EditionType != "remux" {
		t.Errorf("edition = %q/%q, want 2160p/remux", got.EditionLabel, got.EditionType)
	}
	if got.BlobSize == nil || *got.BlobSize != 42949672960 {
		t.Errorf("blob_size = %v, want 42949672960", got.BlobSize)
	}
	if got.BlobMIME == nil || *got.BlobMIME != "video/x-matroska" {
		t.Errorf("blob_mime = %v, want video/x-matroska", got.BlobMIME)
	}
}

// A `linked` asset has no blob at all (ADR-0020), and it must still appear: an
// INNER join to blobs would silently drop every linked file from the one
// listing where a person is counting their files.
func TestWorkAssetsIncludeLinkedFilesWithNoBlob(t *testing.T) {
	h := newHarness(t).seed()

	page := h.workAssets(t, "/api/v1/works/"+work3ID+"/assets")
	if len(page.Items) != 1 {
		t.Fatalf("got %d assets for the book, want 1 (the linked one)", len(page.Items))
	}
	got := page.Items[0]
	if got.BlobHash != nil {
		t.Errorf("blob_hash = %v, want null for a linked asset", *got.BlobHash)
	}
	if got.BlobSize != nil || got.BlobMIME != nil {
		t.Errorf("blob facts = %v/%v, want null for a linked asset", got.BlobSize, got.BlobMIME)
	}
}

// A work with no files is an empty page; a work that does not exist is a 404.
// Both being 200 would make "add something" and "you asked for the wrong thing"
// indistinguishable.
func TestWorkAssetsTellEmptyApartFromUnknown(t *testing.T) {
	h := newHarness(t).seed()

	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, attributes, created_at, updated_at)
		VALUES ('w-empty', 'movie', 'empty|0', 'Empty', 'empty', '{}', ?, ?)`, seedTime, seedTime)

	page := h.workAssets(t, "/api/v1/works/w-empty/assets")
	if len(page.Items) != 0 {
		t.Errorf("got %d assets for a work with none", len(page.Items))
	}

	resp := h.get("/api/v1/works/does-not-exist/assets")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown work = %d, want 404", resp.StatusCode)
	}
}

// The state filter is the one /assets already offers, so a screen can ask for
// the files that are actually there.
func TestWorkAssetsFilterByState(t *testing.T) {
	h := newHarness(t).seed()

	if n := len(h.workAssets(t, "/api/v1/works/"+work2ID+"/assets?state=missing").Items); n != 1 {
		t.Errorf("missing assets of work 2 = %d, want 1", n)
	}
	if n := len(h.workAssets(t, "/api/v1/works/"+work2ID+"/assets?state=present").Items); n != 0 {
		t.Errorf("present assets of work 2 = %d, want 0", n)
	}
}

// The listing pages by keyset like every other collection, and its cursor is
// its own: one from /assets is refused rather than read as a position in a
// different query.
func TestWorkAssetsRefuseAForeignCursor(t *testing.T) {
	h := newHarness(t).seed()

	var assets struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(h.body(h.get("/api/v1/assets?limit=1")), &assets); err != nil {
		t.Fatal(err)
	}
	if assets.NextCursor == "" {
		t.Fatal("no cursor to borrow")
	}
	resp := h.get("/api/v1/works/" + work1ID + "/assets?cursor=" + assets.NextCursor)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a foreign cursor = %d, want 400", resp.StatusCode)
	}
}
