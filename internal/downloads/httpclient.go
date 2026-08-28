package downloads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// HTTPClient is a download client for releases whose source is a direct http(s)
// URL — the §58 case where HEYARR is the client rather than driving a daemon.
//
// # Why it is the Fake's honest sibling
//
// downloads.Fake writes real bytes it was handed; this writes real bytes it
// fetched over HTTP. Everything downstream — the path map, ingest, hashing —
// deals with an ordinary file on disk either way, which is the whole point of
// both being production code the acceptance demo can reach.
//
// # It refuses what is not its to take
//
// A registry Grab tries download clients in order and a refusal falls through
// to the next (registry.Grab). So this client accepts only http(s) sources and
// refuses a magnet URI or an .nzb, which means it composes with a torrent or
// usenet client rather than competing for their transfers — and refusing is
// also just correct: it cannot fetch a magnet.
//
// # The fetch outlives the grab, deliberately
//
// Add returns as soon as the transfer is registered and hands the byte-moving
// to a background goroutine, exactly as a torrent client's Add returns while
// the daemon keeps working. The goroutine therefore does NOT use Add's context
// — that belongs to the grab job, which finishes in milliseconds — but a
// detached context bounded by a generous timeout, so a slow server cannot hang
// a transfer forever and a completed grab does not cancel a download in flight.
type HTTPClient struct {
	name    string
	dir     string // where completed transfers land — the path map's local side
	label   string
	timeout time.Duration
	http    *http.Client
	now     func() time.Time

	mu        sync.Mutex
	transfers map[string]*httpTransfer
}

type httpTransfer struct {
	transfer providers.Transfer
}

// HTTPOptions builds an HTTPClient.
type HTTPOptions struct {
	// Name is the operator's name for this provider.
	Name string
	// Dir is where completed transfers are written — the local side of the
	// provider's path map. Required: a client with nowhere to write would
	// report transfers that ingest could never find.
	Dir string
	// Label tags this instance's transfers. Empty means the default.
	Label string
	// Timeout bounds a single fetch. Zero uses defaultHTTPTimeout.
	Timeout time.Duration
	// HTTP is the client used for fetches. Nil uses a default. Injected so a
	// test drives a fixture server without a network.
	HTTP *http.Client
	// Now is injected so health timestamps are testable.
	Now func() time.Time
}

// defaultHTTPTimeout bounds one fetch. Generous because a release can be large
// on a slow link, finite because a server that stops sending must not pin a
// transfer at DOWNLOADING forever.
const defaultHTTPTimeout = 30 * time.Minute

// NewHTTP builds a plain-HTTP download client, refusing a mis-wired one.
func NewHTTP(opts HTTPOptions) (*HTTPClient, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("downloads: an http client needs a provider name")
	}
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("downloads: an http client needs a download directory (the path_map local side)")
	}
	label := opts.Label
	if label == "" {
		label = DefaultLabel
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	httpClient := opts.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &HTTPClient{
		name:      opts.Name,
		dir:       opts.Dir,
		label:     label,
		timeout:   timeout,
		http:      httpClient,
		now:       now,
		transfers: map[string]*httpTransfer{},
	}, nil
}

var _ providers.Downloader = (*HTTPClient)(nil)

// Name implements providers.Provider.
func (c *HTTPClient) Name() string { return c.name }

// Capabilities implements providers.Provider.
func (c *HTTPClient) Capabilities() []providers.Capability {
	return []providers.Capability{providers.CapabilityDownload}
}

// Check implements providers.Provider.
//
// The client itself is always ready: there is no daemon to be down, and whether
// a particular URL is reachable is discovered at the grab, reported through the
// transfer's Error rather than here. Reporting a version keeps the health shape
// identical to a real client's so a bug in version handling cannot hide behind
// "the http client does not have one".
func (c *HTTPClient) Check(_ context.Context) providers.Health {
	return providers.Healthy("http", c.now())
}

// ErrNotHTTPSource is the refusal that lets this client compose with the
// torrent and usenet clients: a source it cannot fetch falls through to one
// that can.
var ErrNotHTTPSource = errors.New("downloads: not an http(s) source")

// Add implements providers.Downloader — §64's SELECTED → QUEUED edge for a
// direct link.
//
// Idempotent like the fake and the real Transmission client: a job WILL be
// re-run (invariant 9), so a second Add of the same source returns the transfer
// the first created rather than starting a second fetch. The id is derived from
// the source, mirroring a torrent client keying on the infohash.
func (c *HTTPClient) Add(_ context.Context, source secret.Value) (providers.Transfer, error) {
	raw := strings.TrimSpace(source.Reveal())
	if raw == "" {
		return providers.Transfer{}, errors.New("downloads: nothing to add")
	}
	u, err := url.Parse(raw)
	// The source is never echoed: it is a secret.Value that may carry a passkey,
	// and a refusal string reaches an operator's log. The scheme is enough to
	// say why without disclosing the link.
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return providers.Transfer{}, ErrNotHTTPSource
	}

	id := "http:" + sourceKey(source)

	c.mu.Lock()
	if existing, ok := c.transfers[id]; ok {
		t := existing.transfer
		c.mu.Unlock()
		return t, nil
	}
	name := filenameFor(u, id)
	t := providers.Transfer{ID: id, Name: name}
	c.transfers[id] = &httpTransfer{transfer: t}
	c.mu.Unlock()

	//nolint:gosec // G118: the fetch DELIBERATELY outlives the grab job's context — a
	// download continues after Add returns, exactly like a torrent client's daemon; it
	// runs on a detached, timeout-bounded context (see fetch and the type comment).
	go c.fetch(id, raw)
	return t, nil
}

// fetch downloads the bytes to a temp sibling and renames on success, so a
// half-written file never looks complete. It runs on a detached, timeout-bound
// context — see the type comment.
func (c *HTTPClient) fetch(id, rawURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	if err := os.MkdirAll(c.dir, 0o750); err != nil {
		c.failLocked(id, "mkdir: "+err.Error())
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		c.failLocked(id, "request: "+err.Error())
		return
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.failLocked(id, "fetch failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.failLocked(id, fmt.Sprintf("server returned %d", resp.StatusCode))
		return
	}

	c.mu.Lock()
	name := c.transfers[id].transfer.Name
	if resp.ContentLength > 0 {
		c.transfers[id].transfer.BytesTotal = resp.ContentLength
	}
	c.mu.Unlock()

	final := filepath.Join(c.dir, name)
	part := final + ".part"
	//nolint:gosec // G304: name is a sanitised basename (filenameFor — no separators, no
	// traversal) joined under the operator-configured download dir; there is no caller-
	// controlled path here.
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		c.failLocked(id, "create: "+err.Error())
		return
	}

	written, copyErr := io.Copy(&progressWriter{c: c, id: id, dst: f}, resp.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(part)
		c.failLocked(id, "transfer interrupted")
		return
	}
	if err := os.Rename(part, final); err != nil {
		_ = os.Remove(part)
		c.failLocked(id, "finalise: "+err.Error())
		return
	}

	c.mu.Lock()
	t := c.transfers[id]
	t.transfer.Done = true
	t.transfer.BytesDone = written
	if t.transfer.BytesTotal == 0 {
		t.transfer.BytesTotal = written
	}
	t.transfer.Path = final
	c.mu.Unlock()
}

// progressWriter counts bytes as they are written so Transfers can report a
// DOWNLOADING transfer's progress, not just its ends.
type progressWriter struct {
	c   *HTTPClient
	id  string
	dst io.Writer
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if n > 0 {
		w.c.mu.Lock()
		if t, ok := w.c.transfers[w.id]; ok {
			t.transfer.BytesDone += int64(n)
		}
		w.c.mu.Unlock()
	}
	return n, err
}

// failLocked records a transfer's failure. The reason is prefixed the way the
// real client's is so a caller cannot depend on a shape only one produces, and
// it never contains the source URL.
func (c *HTTPClient) failLocked(id, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.transfers[id]; ok {
		t.transfer.Error = string(TroubleClientError) + ": " + detail
	}
}

// Transfers implements providers.Downloader.
func (c *HTTPClient) Transfers(_ context.Context) ([]providers.Transfer, error) {
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
func (c *HTTPClient) Remove(_ context.Context, id string, deleteData bool) error {
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
func (c *HTTPClient) Label() string { return c.label }

// Dir is where completed transfers are written.
func (c *HTTPClient) Dir() string { return c.dir }

// filenameFor derives a safe base filename from the URL, never a path.
//
// The last path segment is what a browser would save, but it is attacker-shaped
// input: "../../etc/passwd" or an empty segment would escape or collide. So it
// is reduced to its base, rejected if it still looks like traversal or is
// empty, and falls back to the transfer id — which is a digest and always safe.
func filenameFor(u *url.URL, id string) string {
	base := path.Base(u.Path)
	if base == "" || base == "." || base == "/" || strings.Contains(base, "..") {
		return strings.ReplaceAll(id, ":", "-") + ".bin"
	}
	// A segment cannot introduce a directory once based, but a stray separator
	// from an odd encoding is cheap to exclude and expensive to debug.
	if strings.ContainsAny(base, `/\`) {
		return strings.ReplaceAll(id, ":", "-") + ".bin"
	}
	return base
}
