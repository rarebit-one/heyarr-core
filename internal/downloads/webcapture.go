package downloads

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// WebCaptureClient is a download client for followed web articles (§58, M12
// Phase 4): the case where the transport does not fetch a file but CAPTURES a
// page — it retrieves an article's HTML and rewrites it into a single,
// self-contained file (ADR-0063).
//
// # Why it is a download client and not something new
//
// Everything downstream of a completed transfer — the path map, ingest, hashing,
// replication — wants an ordinary file on disk, and a self-contained .html IS
// one. So a followed article reaches acquisition as a direct release (the feed
// IS the discovery, ADR-0060) and this client is the §64 SELECTED → QUEUED edge
// for it, the sibling of the http and yt-dlp clients: those move bytes that
// already exist somewhere, this synthesises the bytes it stores from a page and
// its subresources.
//
// # It refuses what is not its to take
//
// A registry Grab tries download clients in order and a refusal falls through to
// the next. This client accepts ONLY a followed.WebCaptureSourceScheme-tagged
// source — the form a KindWebFeed adapter produces — and refuses everything
// else, so it composes with the http, torrent and yt-dlp clients rather than
// competing for their transfers. Routing on the tag rather than on the http(s)
// scheme is what makes the choice independent of registration order: an article
// URL is http(s) and the plain-HTTP client would otherwise claim it and store
// the raw, dependency-laden page (see followed.WebCaptureSourceScheme,
// ADR-0063).
//
// # Self-contained means no external references survive
//
// The captured file inlines each stylesheet as a <style> and each image as a
// data: URI, and DROPS what it cannot inline (a failed subresource, an external
// script): the invariant is that the stored archive references nothing on the
// network, so it renders identically years later when the origin is gone. A
// subresource that will not fetch degrades the archive; it never leaves a live
// URL in it.
//
// # The capture outlives the grab, deliberately
//
// Add returns as soon as the transfer is registered and hands the fetching to a
// background goroutine on a detached, timeout-bounded context, exactly as the
// http and yt-dlp clients do.
type WebCaptureClient struct {
	name    string
	dir     string // where completed captures land — the path map's local side
	label   string
	timeout time.Duration
	http    *http.Client
	now     func() time.Time

	mu        sync.Mutex
	transfers map[string]*webCaptureTransfer
}

type webCaptureTransfer struct {
	transfer providers.Transfer
}

// WebCaptureOptions builds a WebCaptureClient.
type WebCaptureOptions struct {
	// Name is the operator's name for this provider.
	Name string
	// Dir is where completed captures are written — the local side of the
	// provider's path map. Required.
	Dir string
	// Label tags this instance's transfers. Empty means the default.
	Label string
	// Timeout bounds one capture (the page plus its subresources). Zero uses
	// defaultWebCaptureTimeout.
	Timeout time.Duration
	// HTTP is the client used for fetches. Nil uses a default. Injected so a
	// test drives a fixture server without a network.
	HTTP *http.Client
	// Now is injected so health timestamps are testable.
	Now func() time.Time
}

// defaultWebCaptureTimeout bounds one capture. Generous because a page with many
// images on a slow link takes a while, finite because a stuck subresource must
// not pin a transfer at DOWNLOADING forever.
const defaultWebCaptureTimeout = 10 * time.Minute

// Capture bounds. A single article with its assets is well under these; they
// guard a misdirected URL or a hostile page, not a real article.
const (
	maxPageBytes    = 16 << 20 // one HTML document
	maxAssetBytes   = 16 << 20 // one stylesheet or image
	maxAssets       = 256      // how many subresources are inlined at all
	maxCaptureBytes = 64 << 20 // the whole self-contained result
)

// NewWebCapture builds a web-capture download client, refusing a mis-wired one.
func NewWebCapture(opts WebCaptureOptions) (*WebCaptureClient, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, errors.New("downloads: a web-capture client needs a provider name")
	}
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("downloads: a web-capture client needs a download directory (the path_map local side)")
	}
	label := opts.Label
	if label == "" {
		label = DefaultLabel
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultWebCaptureTimeout
	}
	httpClient := opts.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &WebCaptureClient{
		name:      strings.TrimSpace(opts.Name),
		dir:       opts.Dir,
		label:     label,
		timeout:   timeout,
		http:      httpClient,
		now:       now,
		transfers: map[string]*webCaptureTransfer{},
	}, nil
}

var _ providers.Downloader = (*WebCaptureClient)(nil)

// Name implements providers.Provider.
func (c *WebCaptureClient) Name() string { return c.name }

// Capabilities implements providers.Provider.
func (c *WebCaptureClient) Capabilities() []providers.Capability {
	return []providers.Capability{providers.CapabilityDownload}
}

// Check implements providers.Provider. The client itself is always ready:
// whether a particular article is reachable is discovered at the grab, reported
// through the transfer's Error rather than here.
func (c *WebCaptureClient) Check(_ context.Context) providers.Health {
	return providers.Healthy("web-capture", c.now())
}

// ErrNotWebCaptureSource is the refusal that lets this client compose with the
// others: a source that is not web-capture-tagged falls through to one that can
// take it.
var ErrNotWebCaptureSource = errors.New("downloads: not a web-capture source")

// Add implements providers.Downloader — §64's SELECTED → QUEUED edge for a
// followed article.
//
// Idempotent like the other clients (invariant 9): a second Add of the same
// source returns the transfer the first created rather than capturing twice. The
// id is derived from the source.
func (c *WebCaptureClient) Add(_ context.Context, source secret.Value) (providers.Transfer, error) {
	raw := strings.TrimSpace(source.Reveal())
	if raw == "" {
		return providers.Transfer{}, errors.New("downloads: nothing to add")
	}
	if !strings.HasPrefix(raw, followed.WebCaptureSourceScheme) {
		return providers.Transfer{}, ErrNotWebCaptureSource
	}
	article := strings.TrimSpace(strings.TrimPrefix(raw, followed.WebCaptureSourceScheme))
	if article == "" {
		return providers.Transfer{}, errors.New("downloads: a web-capture source has no article URL")
	}

	id := "webcapture:" + sourceKey(source)

	c.mu.Lock()
	if existing, ok := c.transfers[id]; ok {
		t := existing.transfer
		c.mu.Unlock()
		return t, nil
	}
	name := strings.ReplaceAll(id, ":", "-") + ".html"
	t := providers.Transfer{ID: id, Name: name}
	c.transfers[id] = &webCaptureTransfer{transfer: t}
	c.mu.Unlock()

	//nolint:gosec // G118: the capture DELIBERATELY outlives the grab job's context — it
	// continues after Add returns, exactly like a torrent client's daemon; it runs on a
	// detached, timeout-bounded context (see capture and the type comment).
	go c.capture(id, article, name)
	return t, nil
}

// capture fetches the article and its subresources and writes a self-contained
// single-file HTML. It runs on a detached, timeout-bound context — see the type
// comment. It writes to a .part sibling and renames on success, so a
// half-written file never looks complete.
func (c *WebCaptureClient) capture(id, article, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	if err := os.MkdirAll(c.dir, 0o750); err != nil {
		c.failLocked(id, "mkdir: "+err.Error())
		return
	}

	single, err := c.render(ctx, article)
	if err != nil {
		// render's errors are already source-free.
		c.failLocked(id, err.Error())
		return
	}

	final := filepath.Join(c.dir, name)
	part := final + ".part"
	//nolint:gosec // G304: name is a client-derived digest basename (no separators, no
	// traversal) joined under the operator-configured download dir; there is no
	// caller-controlled path here.
	if err := os.WriteFile(part, single, 0o600); err != nil {
		c.failLocked(id, "create: "+err.Error())
		return
	}
	if err := os.Rename(part, final); err != nil {
		_ = os.Remove(part)
		c.failLocked(id, "finalise: "+err.Error())
		return
	}

	c.mu.Lock()
	if t, ok := c.transfers[id]; ok {
		t.transfer.Done = true
		t.transfer.Path = final
		t.transfer.BytesTotal = int64(len(single))
		t.transfer.BytesDone = int64(len(single))
	}
	c.mu.Unlock()
}

// render fetches the article HTML and returns it rewritten to be self-contained.
func (c *WebCaptureClient) render(ctx context.Context, article string) ([]byte, error) {
	base, err := url.Parse(article)
	if err != nil {
		return nil, errors.New("the article URL is not a valid URL")
	}

	page, _, err := c.fetch(ctx, article)
	if err != nil {
		return nil, err
	}
	if len(page) > maxPageBytes {
		return nil, errors.New("the article page is larger than the capture limit")
	}

	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, errors.New("the article page could not be parsed as HTML")
	}

	c.inline(ctx, doc, base)

	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return nil, errors.New("the captured page could not be rendered")
	}
	if buf.Len() > maxCaptureBytes {
		return nil, errors.New("the captured page is larger than the capture limit")
	}
	return buf.Bytes(), nil
}

// inline walks the parsed document and replaces external stylesheets with inline
// <style>, external images with data: URIs, and drops external scripts — so no
// network reference survives. A budget caps how many subresources are inlined,
// after which further ones are dropped rather than fetched, bounding a hostile
// page.
func (c *WebCaptureClient) inline(ctx context.Context, doc *html.Node, base *url.URL) {
	budget := maxAssets
	var toRemove []*html.Node

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "link":
				if isStylesheet(n) {
					if budget > 0 && c.inlineStylesheet(ctx, n, base) {
						budget--
					} else {
						toRemove = append(toRemove, n)
					}
				}
			case "img":
				if budget > 0 && c.inlineImage(ctx, n, base) {
					budget--
				} else {
					// Blank the src rather than remove the element, so alt text
					// and layout survive; a live URL never remains.
					stripAttr(n, "src")
					stripAttr(n, "srcset")
				}
			case "script":
				if getAttr(n, "src") != "" {
					toRemove = append(toRemove, n)
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)

	for _, n := range toRemove {
		if n.Parent != nil {
			n.Parent.RemoveChild(n)
		}
	}
}

// inlineStylesheet fetches a <link rel=stylesheet> and rewrites the node into an
// inline <style>. It reports whether it inlined anything: a failed fetch returns
// false so the caller drops the node rather than leaving a live href.
func (c *WebCaptureClient) inlineStylesheet(ctx context.Context, n *html.Node, base *url.URL) bool {
	href := getAttr(n, "href")
	if href == "" {
		return false
	}
	resolved, ok := resolve(base, href)
	if !ok {
		return false
	}
	body, _, err := c.fetch(ctx, resolved)
	if err != nil || len(body) > maxAssetBytes {
		return false
	}
	// Rewrite the void <link> into a <style> holding the CSS text.
	n.Data = "style"
	n.Attr = nil
	for n.FirstChild != nil {
		n.RemoveChild(n.FirstChild)
	}
	n.AppendChild(&html.Node{Type: html.TextNode, Data: string(body)})
	return true
}

// inlineImage fetches an <img src> and rewrites src to a data: URI. It reports
// whether it inlined anything.
func (c *WebCaptureClient) inlineImage(ctx context.Context, n *html.Node, base *url.URL) bool {
	src := getAttr(n, "src")
	if src == "" || strings.HasPrefix(src, "data:") {
		return src != "" // an existing data: URI is already self-contained.
	}
	resolved, ok := resolve(base, src)
	if !ok {
		return false
	}
	body, contentType, err := c.fetch(ctx, resolved)
	if err != nil || len(body) > maxAssetBytes {
		return false
	}
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	// A srcset would re-introduce external URLs the browser prefers over src.
	stripAttr(n, "srcset")
	setAttr(n, "src", "data:"+contentType+";base64,"+base64.StdEncoding.EncodeToString(body))
	return true
}

// fetch GETs a URL and returns its body and content type. The URL is never
// echoed in an error — a private feed's article or asset URL can carry a token.
func (c *WebCaptureClient) fetch(ctx context.Context, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", errors.New("could not build a request for a resource")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", errors.New("a resource could not be fetched")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("a resource returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetBytes+1))
	if err != nil {
		return nil, "", errors.New("a resource could not be read")
	}
	ct := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	return body, ct, nil
}

// failLocked records a transfer's failure, prefixed the way the other clients'
// are and never containing a URL.
func (c *WebCaptureClient) failLocked(id, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.transfers[id]; ok {
		t.transfer.Error = string(TroubleClientError) + ": " + detail
	}
}

// Transfers implements providers.Downloader.
func (c *WebCaptureClient) Transfers(_ context.Context) ([]providers.Transfer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]providers.Transfer, 0, len(c.transfers))
	for _, t := range c.transfers {
		out = append(out, t.transfer)
	}
	sortTransfers(out)
	return out, nil
}

// Remove drops a transfer, refusing one it does not hold.
func (c *WebCaptureClient) Remove(_ context.Context, id string, deleteData bool) error {
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
func (c *WebCaptureClient) Label() string { return c.label }

// Dir is where completed captures are written.
func (c *WebCaptureClient) Dir() string { return c.dir }

// --- small HTML helpers ---

func isStylesheet(n *html.Node) bool {
	rel := strings.ToLower(getAttr(n, "rel"))
	for _, f := range strings.Fields(rel) {
		if f == "stylesheet" {
			return true
		}
	}
	return false
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func setAttr(n *html.Node, key, val string) {
	for i := range n.Attr {
		if n.Attr[i].Key == key {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

func stripAttr(n *html.Node, key string) {
	out := n.Attr[:0]
	for _, a := range n.Attr {
		if a.Key != key {
			out = append(out, a)
		}
	}
	n.Attr = out
}

// resolve turns a possibly-relative reference into an absolute http(s) URL
// against the page's base. A non-http(s) reference (mailto:, javascript:, an
// already-inlined data:) is refused so nothing but a fetchable resource is ever
// requested.
func resolve(base *url.URL, ref string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", false
	}
	abs := base.ResolveReference(u)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return "", false
	}
	return abs.String(), true
}
