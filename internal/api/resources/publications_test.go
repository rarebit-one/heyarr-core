// Responses in this file are closed by the harness's t.Cleanup.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type publication struct {
	AssetID      string `json:"asset_id"`
	BlobHash     string `json:"blob_hash"`
	Format       string `json:"format"`
	PageCount    *int64 `json:"page_count"`
	ChapterCount *int64 `json:"chapter_count"`
	ContentURL   string `json:"content_url"`
	Size         int64  `json:"size"`
}

func (h *harness) publications(t *testing.T, query string) []publication {
	t.Helper()
	var page struct {
		Items []publication `json:"items"`
	}
	if err := json.Unmarshal(h.body(h.get("/api/v1/publications"+query)), &page); err != nil {
		t.Fatal(err)
	}
	return page.Items
}

func TestPublicationsAreListedWithWhatTheirContainerDeclared(t *testing.T) {
	h := newHarness(t).seed()
	items := h.publications(t, "")
	if len(items) == 0 {
		t.Fatal("no publications")
	}

	byFormat := map[string]publication{}
	for _, p := range items {
		byFormat[p.Format] = p
	}

	epub, ok := byFormat["epub"]
	if !ok {
		t.Fatal("the seeded epub is not listed")
	}
	if epub.ChapterCount == nil || *epub.ChapterCount != 12 {
		t.Errorf("epub chapters = %v, want 12", epub.ChapterCount)
	}
	// A spine is not a page count and must not be reported as one.
	if epub.PageCount != nil {
		t.Errorf("the epub reports a page count of %d", *epub.PageCount)
	}

	// The refusal §69 is built on, visible at the API: a PDF is catalogued,
	// served and readable, and reports no length. Absent, not zero — "0 pages"
	// would be a false statement about a document nobody counted.
	pdf, ok := byFormat["pdf"]
	if !ok {
		t.Fatal("the seeded pdf is not listed")
	}
	if pdf.PageCount != nil || pdf.ChapterCount != nil {
		t.Errorf("the pdf reports counts: %+v", pdf)
	}
	raw := string(h.body(h.get("/api/v1/publications")))
	if strings.Contains(raw, `"page_count":0`) {
		t.Errorf("an unread count was rendered as zero:\n%s", raw)
	}
}

// The bytes come from the ordinary blob endpoint. ADR-0013 is one endpoint with
// four consumers, and a reader is the fifth — not a fifth endpoint.
func TestAPublicationPointsAtTheOrdinaryBlobEndpoint(t *testing.T) {
	h := newHarness(t).seed()
	items := h.publications(t, "")

	for _, p := range items {
		want := "/api/v1/blobs/" + p.BlobHash + "/content"
		if p.ContentURL != want {
			t.Errorf("content_url = %q, want %q", p.ContentURL, want)
		}
		if strings.Contains(p.ContentURL, "publication") {
			t.Errorf("a publication-specific byte route appeared: %q", p.ContentURL)
		}
	}
}

func TestPublicationsFilterByFormat(t *testing.T) {
	h := newHarness(t).seed()

	epubs := h.publications(t, "?format=epub")
	if len(epubs) != 1 || epubs[0].Format != "epub" {
		t.Fatalf("format=epub returned %d items", len(epubs))
	}
	if got := h.publications(t, "?format=cbr"); len(got) != 0 {
		t.Errorf("format=cbr returned %d items", len(got))
	}
	// An unknown filter value is a 400, never a silent "everything": a client
	// that asks for cb7 and gets the whole shelf believes the answer.
	if resp := h.get("/api/v1/publications?format=cb7"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("format=cb7 = %d, want 400", resp.StatusCode)
	}
}

func TestOnePublicationByAssetID(t *testing.T) {
	h := newHarness(t).seed()
	items := h.publications(t, "?format=epub")
	if len(items) != 1 {
		t.Fatal("expected one epub")
	}

	var got publication
	resp := h.get("/api/v1/publications/" + items[0].AssetID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	if got.AssetID != items[0].AssetID || got.Format != "epub" {
		t.Errorf("got %+v", got)
	}

	// Keyed by asset, not blob: one blob can be two assets with two filenames
	// in two libraries, and "which publication" is a question about a use of
	// the bytes.
	if resp := h.get("/api/v1/publications/" + got.BlobHash); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a blob hash resolved as a publication id: %d", resp.StatusCode)
	}
	if resp := h.get("/api/v1/publications/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown publication = %d, want 404", resp.StatusCode)
	}
}
