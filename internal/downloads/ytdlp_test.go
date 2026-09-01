package downloads

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The yt-dlp client's byte-moving runs the real tool and is never exercised in
// CI (ADR-0026); these tests inject a fake Runner to prove the Add → register →
// complete wiring — the same seam HTTPClient's fixture-server tests prove — while
// keeping the subprocess out of the build.

// fakeRunner stands in for the yt-dlp subprocess. It records how many times it
// ran and returns a configured path or error.
type fakeRunner struct {
	calls   atomic.Int64
	path    string
	err     error
	lastURL atomic.Value // string
}

func (f *fakeRunner) Run(_ context.Context, _, _, url string) (string, error) {
	f.calls.Add(1)
	f.lastURL.Store(url)
	return f.path, f.err
}

func newYtDlpClient(t *testing.T, r Runner) *YtDlpClient {
	t.Helper()
	c, err := NewYtDlp(YtDlpOptions{Name: "yt-dlp-under-test", Dir: t.TempDir(), Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// waitYtDlp polls until the transfer finishes or errors, the way the acquisition
// beat does, and returns its final state.
func waitYtDlp(t *testing.T, c *YtDlpClient, id string) providers.Transfer {
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

// ytdlpSource is a source in the form a KindYoutube adapter produces.
func ytdlpSource(watch string) secret.Value {
	return secret.Value(followed.YtDlpSourceScheme + watch)
}

// A source that is not yt-dlp-tagged is refused, so the client composes with the
// http and torrent clients rather than claiming a transfer that is not its —
// even a plain youtube.com URL, which is http(s) and belongs to the HTTP client
// unless a KindYoutube adapter tagged it.
func TestRefusesANonYtDlpSource(t *testing.T) {
	c := newYtDlpClient(t, &fakeRunner{})
	for _, src := range []string{
		"https://www.youtube.com/watch?v=abc",
		"magnet:?xt=urn:btih:deadbeef",
		"https://cdn.example.com/ep1.mp3",
	} {
		if _, err := c.Add(context.Background(), secret.Value(src)); !errors.Is(err, ErrNotYtDlpSource) {
			t.Errorf("Add(%q) error = %v, want ErrNotYtDlpSource", src, err)
		}
	}
}

// A tagged source is fetched by the runner, and the finished file's path lands on
// the transfer so ingest can find it. Add returns before the fetch completes,
// like a real client's queue.
func TestFetchesViaTheRunner(t *testing.T) {
	r := &fakeRunner{path: "/downloads/vid00000001.mp4"}
	c := newYtDlpClient(t, r)

	added, err := c.Add(context.Background(), ytdlpSource("https://www.youtube.com/watch?v=vid00000001"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.Done {
		t.Fatal("Add must return before the download completes")
	}

	final := waitYtDlp(t, c, added.ID)
	if final.Error != "" {
		t.Fatalf("transfer errored: %s", final.Error)
	}
	if !final.Done {
		t.Fatal("transfer did not complete")
	}
	if final.Path != "/downloads/vid00000001.mp4" {
		t.Errorf("path = %q, want the runner's finished file", final.Path)
	}
	// The runner is handed the bare watch URL — the transport tag is stripped
	// before it reaches the tool.
	if got, _ := r.lastURL.Load().(string); got != "https://www.youtube.com/watch?v=vid00000001" {
		t.Errorf("runner got url %q, want the untagged watch URL", got)
	}
}

// yt-dlp's absence (or any run failure) degrades gracefully: the transfer carries
// a named error rather than the process crashing or the client refusing to
// construct.
func TestReportsARunnerFailureOnTheTransfer(t *testing.T) {
	r := &fakeRunner{err: errors.New("yt-dlp not found on PATH")}
	c := newYtDlpClient(t, r)

	added, err := c.Add(context.Background(), ytdlpSource("https://www.youtube.com/watch?v=missing"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	final := waitYtDlp(t, c, added.ID)
	if final.Done {
		t.Error("a failed download must not report Done")
	}
	if final.Error == "" {
		t.Fatal("a runner failure must surface on the transfer's Error")
	}
}

// Add is idempotent (invariant 9): a re-run returns the existing transfer and
// does not start a second download.
func TestAddIsIdempotent(t *testing.T) {
	r := &fakeRunner{path: "/downloads/v.mp4"}
	c := newYtDlpClient(t, r)
	src := ytdlpSource("https://www.youtube.com/watch?v=v")

	first, err := c.Add(context.Background(), src)
	if err != nil {
		t.Fatalf("first Add: %v", err)
	}
	waitYtDlp(t, c, first.ID)

	second, err := c.Add(context.Background(), src)
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("re-Add returned a different transfer: %q vs %q", second.ID, first.ID)
	}
	if got := r.calls.Load(); got != 1 {
		t.Errorf("runner ran %d times, want 1 — a re-Add must not download again", got)
	}
}

// An empty source is refused outright.
func TestRefusesAnEmptySource(t *testing.T) {
	c := newYtDlpClient(t, &fakeRunner{})
	if _, err := c.Add(context.Background(), secret.Value("")); err == nil {
		t.Fatal("an empty source must be refused")
	}
	// A tag with no URL after it is likewise nothing to download.
	if _, err := c.Add(context.Background(), secret.Value(followed.YtDlpSourceScheme)); err == nil {
		t.Fatal("a tag with no video URL must be refused")
	}
}

// The client declares exactly the download capability.
func TestCapabilityIsDownloadOnly(t *testing.T) {
	caps := newYtDlpClient(t, &fakeRunner{}).Capabilities()
	if len(caps) != 1 || caps[0] != providers.CapabilityDownload {
		t.Errorf("capabilities = %v, want [download]", caps)
	}
}

// A client with no download directory is refused at construction: it would
// report transfers ingest could never find.
func TestNewYtDlpRefusesNoDir(t *testing.T) {
	if _, err := NewYtDlp(YtDlpOptions{Name: "x"}); err == nil {
		t.Fatal("a yt-dlp client with no dir must be refused")
	}
}
