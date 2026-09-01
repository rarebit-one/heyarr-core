package downloads

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// YtDlpClient is a download client for YouTube videos (§58, M12 Phase 3): the
// case where the transport is a SUBPROCESS — the external `yt-dlp` tool — rather
// than an HTTP fetch or a torrent daemon.
//
// # Why it is a download client and not something new
//
// Everything downstream of a completed transfer — the path map, ingest, hashing —
// wants an ordinary file on disk, which is exactly what yt-dlp produces. So a
// video that has been followed reaches acquisition as a direct release (the
// channel feed IS the discovery, ADR-0060) and this client is the §64 SELECTED →
// QUEUED edge for it, the honest sibling of HTTPClient: that one writes bytes it
// fetched over HTTP, this writes bytes yt-dlp fetched.
//
// # It refuses what is not its to take
//
// A registry Grab tries download clients in order and a refusal falls through to
// the next. This client accepts ONLY a followed.YtDlpSourceScheme-tagged source —
// the form a KindYoutube feed adapter produces — and refuses everything else, so
// it composes with the http and torrent clients rather than competing for their
// transfers. Routing on the tag rather than on the youtube.com host is what makes
// the choice independent of the order the clients happen to be registered in: a
// bare watch URL is http(s) and the plain-HTTP client would claim it first (see
// followed.YtDlpSourceScheme, ADR-0062).
//
// # yt-dlp is a system dependency, and its absence degrades gracefully
//
// The tool is expected on PATH, not vendored or containerised (plan §8). A host
// without it does not stop the client from constructing or registering
// transfers — the transfer simply fails with a named error the operator can act
// on — exactly as an unreachable HTTP server surfaces through a transfer's Error
// rather than a construction failure.
//
// # The fetch outlives the grab, deliberately
//
// Add returns as soon as the transfer is registered and hands the byte-moving to
// a background goroutine, exactly as HTTPClient and a torrent client's Add do.
// The goroutine therefore does NOT use Add's context — that belongs to the grab
// job, which finishes in milliseconds — but a detached context bounded by a
// generous timeout, so a slow download cannot hang forever and a completed grab
// does not cancel a download in flight.
type YtDlpClient struct {
	name    string
	dir     string // where completed transfers land — the path map's local side
	label   string
	timeout time.Duration
	run     Runner
	now     func() time.Time

	mu        sync.Mutex
	transfers map[string]*ytdlpTransfer
}

type ytdlpTransfer struct {
	transfer providers.Transfer
}

// Runner runs yt-dlp for one video and reports where the finished file landed.
//
// It is injected for the same reason HTTPClient injects an *http.Client: it lets
// a test prove the Add → register → complete wiring deterministically without
// the external tool, while production uses the real subprocess. Per ADR-0026 the
// real runner is exercised only against the live tool, never in CI.
type Runner interface {
	// Run fetches url into dir, writing a file whose base name starts with id,
	// and returns the absolute path of the finished file. A non-nil error means
	// nothing usable was produced (the tool was absent, the video was
	// unavailable, the process failed) and the message is safe to log — it must
	// not echo the source, which can carry a token.
	Run(ctx context.Context, dir, id, url string) (path string, err error)
}

// YtDlpOptions builds a YtDlpClient.
type YtDlpOptions struct {
	// Name is the operator's name for this provider.
	Name string
	// Dir is where completed transfers are written — the local side of the
	// provider's path map. Required: a client with nowhere to write would
	// report transfers ingest could never find.
	Dir string
	// Label tags this instance's transfers. Empty means the default.
	Label string
	// Timeout bounds a single download. Zero uses defaultYtDlpTimeout.
	Timeout time.Duration
	// Runner runs yt-dlp. Nil uses the real subprocess runner (execYtDlp).
	Runner Runner
	// Now is injected so health timestamps are testable.
	Now func() time.Time
}

// defaultYtDlpTimeout bounds one download. Generous because a long video on a
// slow link takes a while, finite because a stuck process must not pin a
// transfer at DOWNLOADING forever.
const defaultYtDlpTimeout = 2 * time.Hour

// ytDlpBinary is the tool this client runs. Named once so the "not found"
// degrade path and the invocation cannot disagree about what is expected.
const ytDlpBinary = "yt-dlp"

// NewYtDlp builds a yt-dlp download client, refusing a mis-wired one.
func NewYtDlp(opts YtDlpOptions) (*YtDlpClient, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("downloads: a yt-dlp client needs a provider name")
	}
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("downloads: a yt-dlp client needs a download directory (the path_map local side)")
	}
	label := opts.Label
	if label == "" {
		label = DefaultLabel
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultYtDlpTimeout
	}
	runner := opts.Runner
	if runner == nil {
		runner = execYtDlp{}
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &YtDlpClient{
		name:      strings.TrimSpace(opts.Name),
		dir:       opts.Dir,
		label:     label,
		timeout:   timeout,
		run:       runner,
		now:       now,
		transfers: map[string]*ytdlpTransfer{},
	}, nil
}

var _ providers.Downloader = (*YtDlpClient)(nil)

// Name implements providers.Provider.
func (c *YtDlpClient) Name() string { return c.name }

// Capabilities implements providers.Provider.
func (c *YtDlpClient) Capabilities() []providers.Capability {
	return []providers.Capability{providers.CapabilityDownload}
}

// Check implements providers.Provider.
//
// The client itself is always ready: there is no daemon to be down, and whether
// yt-dlp is present or a particular video is reachable is discovered at the
// grab, reported through the transfer's Error rather than here — the same stance
// HTTPClient takes on a URL's reachability. Reporting a version keeps the health
// shape identical to a real client's.
func (c *YtDlpClient) Check(_ context.Context) providers.Health {
	return providers.Healthy("yt-dlp", c.now())
}

// ErrNotYtDlpSource is the refusal that lets this client compose with the http
// and torrent clients: a source that is not yt-dlp-tagged falls through to one
// that can take it.
var ErrNotYtDlpSource = errors.New("downloads: not a yt-dlp source")

// Add implements providers.Downloader — §64's SELECTED → QUEUED edge for a
// followed YouTube video.
//
// Idempotent like the http and torrent clients: a job WILL be re-run (invariant
// 9), so a second Add of the same source returns the transfer the first created
// rather than starting a second download. The id is derived from the source,
// mirroring a torrent client keying on the infohash.
func (c *YtDlpClient) Add(_ context.Context, source secret.Value) (providers.Transfer, error) {
	raw := strings.TrimSpace(source.Reveal())
	if raw == "" {
		return providers.Transfer{}, errors.New("downloads: nothing to add")
	}
	// Route on the tag, never on a parsed host: the tag is what a KindYoutube
	// adapter attaches precisely so this decision does not depend on registration
	// order (followed.YtDlpSourceScheme). The source is never echoed in the
	// refusal — it is a secret.Value that may carry a token.
	if !strings.HasPrefix(raw, followed.YtDlpSourceScheme) {
		return providers.Transfer{}, ErrNotYtDlpSource
	}
	watch := strings.TrimSpace(strings.TrimPrefix(raw, followed.YtDlpSourceScheme))
	if watch == "" {
		return providers.Transfer{}, errors.New("downloads: a yt-dlp source has no video URL")
	}

	id := "ytdlp:" + sourceKey(source)

	c.mu.Lock()
	if existing, ok := c.transfers[id]; ok {
		t := existing.transfer
		c.mu.Unlock()
		return t, nil
	}
	t := providers.Transfer{ID: id, Name: watch}
	c.transfers[id] = &ytdlpTransfer{transfer: t}
	c.mu.Unlock()

	//nolint:gosec // G118: the download DELIBERATELY outlives the grab job's context — it
	// continues after Add returns, exactly like a torrent client's daemon; it runs on a
	// detached, timeout-bounded context (see fetch and the type comment).
	go c.fetch(id, watch)
	return t, nil
}

// fetch runs yt-dlp on a detached, timeout-bound context and records where the
// finished file landed. It is the goroutine Add hands the byte-moving to — see
// the type comment.
func (c *YtDlpClient) fetch(id, watch string) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	if err := os.MkdirAll(c.dir, 0o750); err != nil {
		c.failLocked(id, "mkdir: "+err.Error())
		return
	}

	path, err := c.run.Run(ctx, c.dir, strings.ReplaceAll(id, ":", "-"), watch)
	if err != nil {
		// The runner's error is already source-free (see Runner.Run); prefix it
		// the way the other clients prefix theirs so a caller cannot depend on a
		// shape only one produces.
		c.failLocked(id, err.Error())
		return
	}

	c.mu.Lock()
	if t, ok := c.transfers[id]; ok {
		t.transfer.Done = true
		t.transfer.Path = path
	}
	c.mu.Unlock()
}

// failLocked records a transfer's failure. The reason is prefixed the way the
// other clients' are so a caller cannot depend on a shape only one produces, and
// it never contains the source URL.
func (c *YtDlpClient) failLocked(id, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.transfers[id]; ok {
		t.transfer.Error = string(TroubleClientError) + ": " + detail
	}
}

// Transfers implements providers.Downloader.
func (c *YtDlpClient) Transfers(_ context.Context) ([]providers.Transfer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]providers.Transfer, 0, len(c.transfers))
	for _, t := range c.transfers {
		out = append(out, t.transfer)
	}
	sortTransfers(out)
	return out, nil
}

// Remove drops a transfer, refusing one it does not hold — the same refusal the
// other clients make, so nothing can reach past what this client queued.
func (c *YtDlpClient) Remove(_ context.Context, id string, deleteData bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.transfers[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotOurs, id)
	}
	if deleteData && t.transfer.Path != "" {
		if err := os.Remove(t.transfer.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	delete(c.transfers, id)
	return nil
}

// Label reports what this client tags its transfers with.
func (c *YtDlpClient) Label() string { return c.label }

// Dir is where completed transfers are written.
func (c *YtDlpClient) Dir() string { return c.dir }

// execYtDlp is the production Runner: it runs the real yt-dlp subprocess.
//
// Per ADR-0026 it is never exercised in CI — the unit tests inject a fake Runner
// — so what is proven here is the invocation, not a live download.
type execYtDlp struct{}

// Run invokes yt-dlp to fetch one video into dir and returns the finished file's
// path.
//
// yt-dlp writes to `<dir>/<id>.<ext>` (the extension is whatever it muxes to)
// and `--print after_move:filepath` makes it print that final path once the file
// is in place, which is read back as the transfer's Path. `--no-playlist` keeps
// a "watch?v=…&list=…" URL to the single video. The tool's absence is reported
// as a named, source-free error so the transfer's Error tells an operator to
// install it rather than surfacing a bare exec failure.
func (execYtDlp) Run(ctx context.Context, dir, id, url string) (string, error) {
	if _, err := exec.LookPath(ytDlpBinary); err != nil {
		return "", fmt.Errorf("%s not found on PATH (install it, or this source cannot be downloaded)", ytDlpBinary)
	}
	outTemplate := dir + string(os.PathSeparator) + id + ".%(ext)s"
	//nolint:gosec // G204: the only caller-influenced argument is the video URL, passed
	// as a single argv element (no shell); dir and id are heyarr-derived (the configured
	// download dir and a digest-based transfer id), and the binary name is a constant.
	cmd := exec.CommandContext(ctx, ytDlpBinary,
		"--no-playlist",
		"--no-progress",
		"--quiet",
		"--print", "after_move:filepath",
		"-o", outTemplate,
		url,
	)
	out, err := cmd.Output()
	if err != nil {
		// Never include err (it can quote the command line, which carries the
		// URL) — a generic, actionable message is enough for an operator's log.
		return "", errors.New("yt-dlp failed to fetch the video")
	}
	path := lastNonEmptyLine(string(out))
	if path == "" {
		return "", errors.New("yt-dlp produced no output file")
	}
	return path, nil
}

// lastNonEmptyLine returns the final non-blank line of s. yt-dlp's
// after_move:filepath print is one line, but guarding against trailing
// whitespace and any stray line keeps the path read robust.
func lastNonEmptyLine(s string) string {
	var last string
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			last = line
		}
	}
	return last
}
