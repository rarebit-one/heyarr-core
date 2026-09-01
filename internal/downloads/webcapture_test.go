package downloads

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The web-capture client is pure HTTP, so unlike the yt-dlp client its capture
// IS exercised in CI against fixture servers (ADR-0026 forbids a live external
// service, not an httptest one). These tests prove the whole Add → capture →
// self-contained-file arc.

// captureFixture serves an article page, a stylesheet and an image, so a capture
// has real subresources to inline.
func captureFixture(t *testing.T) *httptest.Server {
	t.Helper()
	// A 1x1 transparent PNG.
	png := []byte("\x89PNG\r\n\x1a\n_the_real_png_bytes_")
	mux := http.NewServeMux()
	mux.HandleFunc("/style.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte("body{color:rebeccapurple}"))
	})
	mux.HandleFunc("/pic.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	})
	mux.HandleFunc("/app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("console.log('tracker')"))
	})
	mux.HandleFunc("/article", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><head>
<link rel="stylesheet" href="/style.css">
<script src="/app.js"></script>
</head><body><h1>Headline</h1>
<img src="/pic.png" alt="a picture">
<img src="/missing.png" alt="gone">
</body></html>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newWebCaptureClient(t *testing.T) *WebCaptureClient {
	t.Helper()
	c, err := NewWebCapture(WebCaptureOptions{Name: "web-capture-under-test", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func waitWebCapture(t *testing.T, c *WebCaptureClient, id string) providers.Transfer {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		transfers, err := c.Transfers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range transfers {
			if tr.ID == id && (tr.Done || tr.Error != "") {
				return tr
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("transfer %s never finished", id)
	return providers.Transfer{}
}

func webCaptureSource(article string) secret.Value {
	return secret.Value(followed.WebCaptureSourceScheme + article)
}

// A source that is not web-capture-tagged is refused, so the client composes
// with the other clients — even a plain article URL, which is http(s) and
// belongs to the HTTP client unless a KindWebFeed adapter tagged it.
func TestRefusesNonWebCaptureSource(t *testing.T) {
	c := newWebCaptureClient(t)
	for _, src := range []string{
		"https://journal.example.com/article",
		"magnet:?xt=urn:btih:deadbeef",
		"ytdlp:https://www.youtube.com/watch?v=x",
	} {
		if _, err := c.Add(context.Background(), secret.Value(src)); !errors.Is(err, ErrNotWebCaptureSource) {
			t.Errorf("Add(%q) error = %v, want ErrNotWebCaptureSource", src, err)
		}
	}
}

// The captured file is self-contained: the stylesheet is inlined, the image is a
// data: URI, the external script is dropped, and NO reference to the origin
// survives — so it renders when the origin is gone.
func TestCapturesSelfContainedHTML(t *testing.T) {
	srv := captureFixture(t)
	c := newWebCaptureClient(t)

	added, err := c.Add(context.Background(), webCaptureSource(srv.URL+"/article"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.Done {
		t.Fatal("Add must return before the capture completes")
	}
	final := waitWebCapture(t, c, added.ID)
	if final.Error != "" {
		t.Fatalf("capture errored: %s", final.Error)
	}
	if !final.Done || final.Path == "" {
		t.Fatalf("capture did not complete: %+v", final)
	}

	raw, err := os.ReadFile(final.Path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)

	if strings.Contains(out, srv.URL) {
		t.Errorf("the archive still references the origin %q — not self-contained:\n%s", srv.URL, out)
	}
	if strings.Contains(out, "<link") {
		t.Error("the external stylesheet link was not replaced by an inline <style>")
	}
	if !strings.Contains(out, "rebeccapurple") {
		t.Error("the stylesheet CSS was not inlined")
	}
	if !strings.Contains(out, "data:image/png;base64,") {
		t.Error("the image was not inlined as a data: URI")
	}
	if strings.Contains(out, "app.js") {
		t.Error("the external script was not dropped")
	}
	// The unfetchable image (/missing.png) must not leave a live URL behind.
	if strings.Contains(out, "missing.png") {
		t.Error("an unfetchable image left a live URL in the archive")
	}
	// The headline text survives.
	if !strings.Contains(out, "Headline") {
		t.Error("the article content was lost")
	}
}

// Add is idempotent (invariant 9): a re-run returns the existing transfer.
func TestWebCaptureAddIsIdempotent(t *testing.T) {
	srv := captureFixture(t)
	c := newWebCaptureClient(t)
	src := webCaptureSource(srv.URL + "/article")

	first, err := c.Add(context.Background(), src)
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}
	waitWebCapture(t, c, first.ID)

	second, err := c.Add(context.Background(), src)
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("re-Add returned a different transfer: %q vs %q", second.ID, first.ID)
	}
}

// An article page that will not fetch surfaces on the transfer's Error rather
// than crashing or producing an empty file.
func TestReportsAFetchFailure(t *testing.T) {
	srv := captureFixture(t)
	c := newWebCaptureClient(t)
	added, err := c.Add(context.Background(), webCaptureSource(srv.URL+"/does-not-exist"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	final := waitWebCapture(t, c, added.ID)
	if final.Done {
		t.Error("a failed capture must not report Done")
	}
	if final.Error == "" {
		t.Fatal("a fetch failure must surface on the transfer's Error")
	}
}

// An empty or tag-only source is refused.
func TestWebCaptureRefusesEmptySource(t *testing.T) {
	c := newWebCaptureClient(t)
	if _, err := c.Add(context.Background(), secret.Value("")); err == nil {
		t.Fatal("an empty source must be refused")
	}
	if _, err := c.Add(context.Background(), secret.Value(followed.WebCaptureSourceScheme)); err == nil {
		t.Fatal("a tag with no article URL must be refused")
	}
}

// The client declares exactly the download capability.
func TestWebCaptureCapabilityIsDownloadOnly(t *testing.T) {
	caps := newWebCaptureClient(t).Capabilities()
	if len(caps) != 1 || caps[0] != providers.CapabilityDownload {
		t.Errorf("capabilities = %v, want [download]", caps)
	}
}

// A client with no download directory is refused at construction.
func TestNewWebCaptureRefusesNoDir(t *testing.T) {
	if _, err := NewWebCapture(WebCaptureOptions{Name: "x"}); err == nil {
		t.Fatal("a web-capture client with no dir must be refused")
	}
}
