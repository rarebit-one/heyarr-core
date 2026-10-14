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

// webCaptureFeedSource builds the new source format that carries both the
// article URL and the feed URL, exactly as providers/webfeed.Enumerate emits.
func webCaptureFeedSource(article, feedURL string) secret.Value {
	return secret.Value(followed.WebCaptureSourceScheme + article + "\x00feed:" + feedURL)
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

// A feed host whose WAF returns 404 to Go's default User-Agent but 200 to a
// descriptive heyarr/… UA must still yield a successful capture.
func TestWebCaptureSendsDescriptiveUserAgent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "heyarr/") {
			// Simulate a WAF that rejects Go's default UA.
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Article</h1></body></html>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newWebCaptureClient(t)
	added, err := c.Add(context.Background(), webCaptureSource(srv.URL+"/article"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	final := waitWebCapture(t, c, added.ID)
	if final.Error != "" {
		t.Fatalf("capture errored against a UA-gating server: %s", final.Error)
	}
	if !final.Done {
		t.Fatal("capture did not complete against a UA-gating server")
	}
}

// rssWithContent is a minimal RSS feed whose single item carries a full
// content:encoded body, so the fallback path can succeed.
const rssWithContent = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/">
  <channel>
    <item>
      <title>Blocked Article</title>
      <link>ARTICLE_URL</link>
      <guid isPermaLink="false">blocked-guid-001</guid>
      <pubDate>Wed, 01 Oct 2026 10:00:00 +0000</pubDate>
      <content:encoded><![CDATA[<p>Full article body from feed.</p>]]></content:encoded>
    </item>
  </channel>
</rss>`

// rssWithDescriptionOnly is a feed whose item carries only a description — the
// fallback must NOT use it (description/summary is never full content).
const rssWithDescriptionOnly = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <item>
      <title>Blocked Article</title>
      <link>ARTICLE_URL</link>
      <guid isPermaLink="false">blocked-guid-001</guid>
      <description>Just a short summary.</description>
    </item>
  </channel>
</rss>`

// feedFallbackFixture builds a test server that acts as both an article host
// and a feed host. articleStatus is what the article endpoint returns;
// feedBody is what the feed endpoint returns (with ARTICLE_URL replaced by the
// article's real URL).
func feedFallbackFixture(t *testing.T, articleStatus int, feedBody string) (articleURL, feedURL string) {
	t.Helper()
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/article", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(articleStatus)
	})
	mux.HandleFunc("/feed", func(w http.ResponseWriter, _ *http.Request) {
		body := strings.ReplaceAll(feedBody, "ARTICLE_URL", srvURL+"/article")
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	srvURL = srv.URL
	return srvURL + "/article", srvURL + "/feed"
}

// When an article page returns 403 and the feed carries content:encoded, the
// capture should succeed using the feed's own HTML body and include the
// provenance meta.
func TestFeedFallbackOn403WithContent(t *testing.T) {
	articleURL, feedURL := feedFallbackFixture(t, http.StatusForbidden, rssWithContent)
	c := newWebCaptureClient(t)
	added, err := c.Add(context.Background(), webCaptureFeedSource(articleURL, feedURL))
	if err != nil {
		t.Fatalf("Add: %v", err)
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
	if !strings.Contains(out, `heyarr-capture-source`) || !strings.Contains(out, `content="feed"`) {
		t.Error("provenance meta tag not present in feed-fallback capture")
	}
	if !strings.Contains(out, "Full article body from feed") {
		t.Error("feed body content not present in capture")
	}
}

// When an article page returns 403 but the feed carries only a description
// (not content:encoded), the capture must fail with the original 403 error and
// not produce a file.
func TestFeedFallbackOn403DescriptionOnly(t *testing.T) {
	articleURL, feedURL := feedFallbackFixture(t, http.StatusForbidden, rssWithDescriptionOnly)
	c := newWebCaptureClient(t)
	added, err := c.Add(context.Background(), webCaptureFeedSource(articleURL, feedURL))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	final := waitWebCapture(t, c, added.ID)
	if final.Done {
		t.Error("capture must not succeed when the feed has only a description")
	}
	if final.Error == "" {
		t.Error("a failed fallback must surface an error on the transfer")
	}
}

// A 5xx response from the article page must NOT attempt the feed fallback —
// server errors are transient and the job will retry.
func TestFeedFallbackNotAttemptedOn5xx(t *testing.T) {
	var feedHits int
	articleStatus := http.StatusInternalServerError
	articleURL := ""
	mux := http.NewServeMux()
	mux.HandleFunc("/article", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(articleStatus)
	})
	mux.HandleFunc("/feed", func(w http.ResponseWriter, _ *http.Request) {
		feedHits++
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(rssWithContent))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	articleURL = srv.URL + "/article"
	feedURL := srv.URL + "/feed"

	c := newWebCaptureClient(t)
	added, err := c.Add(context.Background(), webCaptureFeedSource(articleURL, feedURL))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	waitWebCapture(t, c, added.ID)
	if feedHits != 0 {
		t.Errorf("feed was fetched %d time(s); must not be fetched on a 5xx article response", feedHits)
	}
}

// A 200 article page must work exactly as before — no feed fetch.
func TestFeedSourceWithSuccessfulArticle(t *testing.T) {
	var feedHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/article", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>OK</h1></body></html>`))
	})
	mux.HandleFunc("/feed", func(w http.ResponseWriter, _ *http.Request) {
		feedHits++
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newWebCaptureClient(t)
	added, err := c.Add(context.Background(), webCaptureFeedSource(srv.URL+"/article", srv.URL+"/feed"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	final := waitWebCapture(t, c, added.ID)
	if final.Error != "" {
		t.Fatalf("capture errored: %s", final.Error)
	}
	if !final.Done {
		t.Fatal("capture did not complete")
	}
	if feedHits != 0 {
		t.Errorf("feed was fetched %d time(s); must not be fetched when article succeeds", feedHits)
	}
}

// A legacy source (no feed segment) that encounters a 403 must fail as before —
// no feed fallback attempted.
func TestLegacySourceOn403NoFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	c := newWebCaptureClient(t)
	added, err := c.Add(context.Background(), webCaptureSource(srv.URL+"/article"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	final := waitWebCapture(t, c, added.ID)
	if final.Done {
		t.Error("a 403 on a legacy source must not succeed")
	}
	if final.Error == "" {
		t.Error("a 403 on a legacy source must surface an error")
	}
}

// The source format round-trips: the article and feed URL parsed back from the
// source string match what webfeed.Enumerate would have written.
func TestFeedSourceRoundTrip(t *testing.T) {
	article := "https://journal.example.com/first"
	feedURL := "https://journal.example.com/feed"
	src := string(webCaptureFeedSource(article, feedURL))

	// Must start with the webcapture scheme.
	if !strings.HasPrefix(src, followed.WebCaptureSourceScheme) {
		t.Fatalf("source %q does not start with webcapture scheme", src)
	}
	body := strings.TrimPrefix(src, followed.WebCaptureSourceScheme)
	parts := strings.SplitN(body, "\x00", 2)
	if parts[0] != article {
		t.Errorf("article = %q, want %q", parts[0], article)
	}
	if len(parts) != 2 || parts[1] != "feed:"+feedURL {
		t.Errorf("feed segment = %q, want %q", func() string {
			if len(parts) < 2 {
				return "(missing)"
			}
			return parts[1]
		}(), "feed:"+feedURL)
	}
}
