package worker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// CASArtworkStore adopts fetched cover bytes into the content-addressed store,
// hashing as it writes (cas.Store.Put). The production ArtworkBlobStore.
type CASArtworkStore struct{ store cas.Store }

// NewCASArtworkStore adapts a CAS store for artwork fetching.
func NewCASArtworkStore(store cas.Store) *CASArtworkStore { return &CASArtworkStore{store: store} }

var _ ArtworkBlobStore = (*CASArtworkStore)(nil)

// Put streams the reader into the store and returns the blob's hash and size.
func (s *CASArtworkStore) Put(ctx context.Context, r io.Reader) (string, int64, error) {
	desc, err := s.store.Put(ctx, r)
	if err != nil {
		return "", 0, err
	}
	return desc.Hash.String(), desc.Size, nil
}

// artworkFetchTimeout bounds one cover download. A cover is a few hundred KB, so
// this is a guard against a link that hangs.
const artworkFetchTimeout = 60 * time.Second

// maxArtworkBytes bounds what a cover download will read — a guard against a
// misdirected link, not a real limit.
const maxArtworkBytes = 32 << 20

// HTTPArtworkFetcher fetches a cover image over http(s), following redirects (the
// Cover Art Archive answers /front with a redirect to the image). It reports the
// image type the response carried so the asset records a truthful mime.
type HTTPArtworkFetcher struct{ client *http.Client }

// NewHTTPArtworkFetcher builds the production ArtworkFetcher.
func NewHTTPArtworkFetcher() *HTTPArtworkFetcher {
	return &HTTPArtworkFetcher{client: &http.Client{Timeout: artworkFetchTimeout}}
}

var _ ArtworkFetcher = (*HTTPArtworkFetcher)(nil)

// Fetch downloads the cover at url and returns its bytes and image mime. The URL
// is revealed only here, where it goes on the wire.
func (d *HTTPArtworkFetcher) Fetch(ctx context.Context, u secret.Value) (io.ReadCloser, string, error) {
	raw := u.Reveal()
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("artwork download: unparseable link")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", fmt.Errorf("artwork download: link is not http(s)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", fmt.Errorf("artwork download: building request: %w", err)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("artwork download: request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, "", fmt.Errorf("artwork download: link returned HTTP %d", resp.StatusCode)
	}
	mime := imageMIME(resp.Header.Get("Content-Type"))
	return &limitedReadCloser{r: io.LimitReader(resp.Body, maxArtworkBytes), c: resp.Body}, mime, nil
}

// imageMIME extracts an image content type from a response's Content-Type header,
// dropping any charset parameter. A non-image or absent type yields "" so the
// catalog falls back to its default rather than recording, say, text/html for a
// misdirected link.
func imageMIME(header string) string {
	ct := strings.TrimSpace(header)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	ct = strings.ToLower(ct)
	if strings.HasPrefix(ct, "image/") {
		return ct
	}
	return ""
}
