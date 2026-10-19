// Response bodies are closed by the t.Cleanup the harness registers in get().
//
//nolint:bodyclose // closed by the harness's t.Cleanup
package opds_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

func TestDownloadReturnsExactBytes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	resp := h.get("/opds/download/ea1", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body(t, resp); !bytes.Equal(got, h.bytesOf["ea1"]) {
		t.Fatalf("download body = %q, want the blob bytes %q", got, h.bytesOf["ea1"])
	}
}

func TestSameFormatBookFilesAreSeparatelyDownloadable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.edition(context.Background(), h.casStore, "wa", "ea4", "cbz-volume", "application/x-cbz", []byte("second comic volume"))
	h.exec(`UPDATE assets SET edition_id = 'ea2', filename = 'Volume 2.cbz' WHERE id = 'aea4'`)

	f := parseFeed(t, h.get("/opds/publications", true))
	links := map[string]string{}
	for _, entry := range f.Entries {
		if entry.Title == "The Long Survey" {
			for _, link := range entry.Links {
				links[link.Href] = link.Title
			}
		}
	}
	if len(links) != 3 || links["/opds/download/aea4"] != "Volume 2.cbz" {
		t.Fatalf("acquisition links = %v, want two old links plus the second comic volume", links)
	}
	if _, ok := links["/opds/download/ea1"]; !ok {
		t.Fatalf("the existing EPUB link is missing: %v", links)
	}
	if _, ok := links["/opds/download/ea2"]; !ok {
		t.Fatalf("the existing CBZ link is missing: %v", links)
	}
	for id, want := range map[string][]byte{
		"ea2":  h.bytesOf["ea2"],
		"aea4": h.bytesOf["ea4"],
	} {
		resp := h.get("/opds/download/"+id, true)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body(t, resp), want) {
			t.Fatalf("download %s did not return its own volume", id)
		}
	}
}

// TestDownloadHonoursRange proves byte serving is delegated to the blob handler
// unchanged: a Range request gets a 206 with a Content-Range and only the
// requested bytes. The adapter writes none of that itself.
func TestDownloadHonoursRange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	want := h.bytesOf["ea1"]

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, h.http.URL+"/opds/download/ea1", nil)
	req.SetBasicAuth("reader", h.token)
	req.Header.Set("Range", "bytes=0-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", resp.StatusCode)
	}
	if resp.Header.Get("Content-Range") == "" {
		t.Error("206 is missing Content-Range")
	}
	if got := body(t, resp); !bytes.Equal(got, want[:4]) {
		t.Fatalf("range body = %q, want first four bytes %q", got, want[:4])
	}
}

func TestDownloadNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	resp := h.get("/opds/download/no-such-edition", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestDownloadLinkedNotAcquirable proves the linked-only edition, which the feed
// never advertises, also cannot be downloaded directly — it has no blob.
func TestDownloadLinkedNotAcquirable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	resp := h.get("/opds/download/eb1", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a linked (blob-less) edition", resp.StatusCode)
	}
}

func TestDownloadRequiresAuth(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	resp := h.get("/opds/download/ea1", false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
