package downloads

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// newHTTPClient builds a client writing into a fresh temp dir.
func newHTTPClient(t *testing.T) *HTTPClient {
	t.Helper()
	c, err := NewHTTP(HTTPOptions{Name: "http-under-test", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// waitDone polls the client the way the acquisition beat does, until the
// transfer finishes or errors, and returns its final state.
func waitDone(t *testing.T, c *HTTPClient, id string) providers.Transfer {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
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

func TestHTTPFetchesRealBytes(t *testing.T) {
	content := []byte("the-real-http-payload-0123456789-abcdefghijklmnop")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	c := newHTTPClient(t)
	added, err := c.Add(context.Background(), secret.Value(srv.URL+"/report.pdf"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.Done {
		t.Fatal("Add must return before the fetch completes, like a real client's queue")
	}

	final := waitDone(t, c, added.ID)
	if final.Error != "" {
		t.Fatalf("transfer errored: %s", final.Error)
	}
	if !final.Done {
		t.Fatal("transfer did not complete")
	}
	if final.Name != "report.pdf" {
		t.Errorf("name = %q, want report.pdf (the URL's last segment)", final.Name)
	}
	if final.BytesDone != int64(len(content)) {
		t.Errorf("bytesDone = %d, want %d", final.BytesDone, len(content))
	}

	// The bytes on disk ARE the bytes served — the whole point of a real client.
	got, err := os.ReadFile(final.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("written bytes = %q, want %q", got, content)
	}
	// Nothing half-written left behind.
	if _, err := os.Stat(final.Path + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".part temp file was not cleaned up")
	}
}

func TestHTTPRefusesNonHTTPSource(t *testing.T) {
	c := newHTTPClient(t)
	// A magnet is not this client's to fetch — it must REFUSE so the grab falls
	// through to a torrent client rather than swallowing the transfer.
	_, err := c.Add(context.Background(), secret.Value("magnet:?xt=urn:btih:deadbeef"))
	if !errors.Is(err, ErrNotHTTPSource) {
		t.Fatalf("a magnet should be refused with ErrNotHTTPSource, got %v", err)
	}
	if _, err := c.Add(context.Background(), secret.Value("  ")); err == nil {
		t.Fatal("an empty source should be refused")
	}
	// A refusal must never register a transfer — otherwise a re-run would find a
	// phantom and never try the client that can actually fetch it.
	transfers, _ := c.Transfers(context.Background())
	if len(transfers) != 0 {
		t.Fatalf("a refused source registered %d transfers", len(transfers))
	}
}

func TestHTTPServerErrorIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newHTTPClient(t)
	added, err := c.Add(context.Background(), secret.Value(srv.URL+"/missing.bin"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	final := waitDone(t, c, added.ID)
	if final.Done {
		t.Fatal("a 404 must not complete the transfer")
	}
	if final.Error == "" || !contains(final.Error, "404") {
		t.Fatalf("error should name the status, got %q", final.Error)
	}
	// No file, no leftover temp file for a transfer that never arrived.
	entries, _ := os.ReadDir(c.Dir())
	if len(entries) != 0 {
		t.Fatalf("a failed fetch left files behind: %v", entries)
	}
}

func TestHTTPAddIsIdempotent(t *testing.T) {
	var hits int32
	content := []byte("idempotent-payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	c := newHTTPClient(t)
	source := secret.Value(srv.URL + "/thing.bin")
	first, err := c.Add(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, c, first.ID)

	// A re-run of the grab (invariant 9) must return the same transfer and NOT
	// start a second fetch.
	second, err := c.Add(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("a repeated Add produced a new id: %s vs %s", second.ID, first.ID)
	}
	// A second fetch, if one were wrongly started, would run in a goroutine and
	// reach the (local) server within milliseconds. Watch for that rather than
	// sampling once the instant Add returns — sampling too early is how this
	// exact assertion can pass while proving nothing.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := atomic.LoadInt32(&hits); got != 1 {
			t.Fatalf("a repeated Add started a second fetch: server hit %d times, want 1", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHTTPRemove(t *testing.T) {
	content := []byte("removable")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	c := newHTTPClient(t)
	added, _ := c.Add(context.Background(), secret.Value(srv.URL+"/gone.bin"))
	final := waitDone(t, c, added.ID)

	if err := c.Remove(context.Background(), final.ID, true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(final.Path); !errors.Is(err, os.ErrNotExist) {
		t.Error("Remove with deleteData left the file")
	}
	// A transfer it does not hold is refused — nothing can reach past what this
	// client queued.
	if err := c.Remove(context.Background(), "http:not-ours", false); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("removing a foreign transfer should be ErrNotOurs, got %v", err)
	}
}

func TestHTTPFilenameFor(t *testing.T) {
	cases := []struct{ url, want string }{
		{"http://h/a/b/report.pdf", "report.pdf"},
		{"http://h/movie.mkv", "movie.mkv"},
		{"http://h/", "http-abc.bin"},           // no segment → id fallback
		{"http://h/../../etc/passwd", "passwd"}, // path.Base strips the traversal to a safe basename
		{"http://h/foo/..", "http-abc.bin"},     // a trailing .. segment → id fallback
	}
	for _, tc := range cases {
		u := mustParse(t, tc.url)
		if got := filenameFor(u, "http:abc"); got != tc.want {
			t.Errorf("filenameFor(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestNewHTTPRefusesMisWired(t *testing.T) {
	if _, err := NewHTTP(HTTPOptions{Dir: "/tmp"}); err == nil {
		t.Error("a client with no name should be refused")
	}
	if _, err := NewHTTP(HTTPOptions{Name: "x"}); err == nil {
		t.Error("a client with no download directory should be refused")
	}
}

func TestHTTPConstructRequiresPathMap(t *testing.T) {
	// The constructor refuses a client with nowhere to write, the same shape as
	// the fake — a completed transfer with no home ingest could never find.
	_, handled, err := httpFromConfig(providers.Resolved{Name: "http", Kind: providers.KindHTTP}, time.Now)
	if !handled {
		t.Fatal("httpFromConfig should claim a KindHTTP resolved entry")
	}
	if err == nil {
		t.Fatal("an http client with no path_map should be refused")
	}
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
