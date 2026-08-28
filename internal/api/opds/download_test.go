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
	h := newHarness(t)
	resp := h.get("/opds/download/ea1", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body(t, resp); !bytes.Equal(got, h.bytesOf["ea1"]) {
		t.Fatalf("download body = %q, want the blob bytes %q", got, h.bytesOf["ea1"])
	}
}

// TestDownloadHonoursRange proves byte serving is delegated to the blob handler
// unchanged: a Range request gets a 206 with a Content-Range and only the
// requested bytes. The adapter writes none of that itself.
func TestDownloadHonoursRange(t *testing.T) {
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
	h := newHarness(t)
	resp := h.get("/opds/download/no-such-edition", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestDownloadLinkedNotAcquirable proves the linked-only edition, which the feed
// never advertises, also cannot be downloaded directly — it has no blob.
func TestDownloadLinkedNotAcquirable(t *testing.T) {
	h := newHarness(t)
	resp := h.get("/opds/download/eb1", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a linked (blob-less) edition", resp.StatusCode)
	}
}

func TestDownloadRequiresAuth(t *testing.T) {
	h := newHarness(t)
	resp := h.get("/opds/download/ea1", false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
