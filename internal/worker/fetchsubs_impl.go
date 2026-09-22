package worker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// CASSubtitleStore adopts fetched subtitle bytes into the content-addressed
// store, hashing as it writes (cas.Store.Put). The production SubtitleBlobStore.
type CASSubtitleStore struct{ store cas.Store }

// NewCASSubtitleStore adapts a CAS store for subtitle fetching.
func NewCASSubtitleStore(store cas.Store) *CASSubtitleStore { return &CASSubtitleStore{store: store} }

var _ SubtitleBlobStore = (*CASSubtitleStore)(nil)

// Put streams the reader into the store and returns the blob's hash and size.
func (s *CASSubtitleStore) Put(ctx context.Context, r io.Reader) (string, int64, error) {
	desc, err := s.store.Put(ctx, r)
	if err != nil {
		return "", 0, err
	}
	return desc.Hash.String(), desc.Size, nil
}

// subtitleFetchTimeout bounds one download. A subtitle is a few KB, so this is a
// guard against a link that hangs, not a real budget.
const subtitleFetchTimeout = 60 * time.Second

// maxSubtitleBytes bounds what a download will read. A subtitle file is small;
// this is a guard against a misdirected link, not a real limit.
const maxSubtitleBytes = 16 << 20

// HTTPSubtitleDownloader fetches a resolved subtitle link over http(s). A
// subtitle is small, so this is a plain bounded GET rather than the
// transfer-tracking download client a torrent needs.
type HTTPSubtitleDownloader struct{ client *http.Client }

// NewHTTPSubtitleDownloader builds the production SubtitleDownloader.
func NewHTTPSubtitleDownloader() *HTTPSubtitleDownloader {
	return &HTTPSubtitleDownloader{client: &http.Client{Timeout: subtitleFetchTimeout}}
}

var _ SubtitleDownloader = (*HTTPSubtitleDownloader)(nil)

// Fetch downloads the bytes at url. The URL is revealed only here, where it is
// put on the wire; it is a per-fetch token and must not reach a log.
func (d *HTTPSubtitleDownloader) Fetch(ctx context.Context, u secret.Value) (io.ReadCloser, error) {
	raw := u.Reveal()
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("subtitle download: unparseable link")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("subtitle download: link is not http(s)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, fmt.Errorf("subtitle download: building request: %w", err)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("subtitle download: request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("subtitle download: link returned HTTP %d", resp.StatusCode)
	}
	return &limitedReadCloser{r: io.LimitReader(resp.Body, maxSubtitleBytes), c: resp.Body}, nil
}

// limitedReadCloser bounds the bytes read while still closing the underlying
// response body.
type limitedReadCloser struct {
	r io.Reader
	c io.Closer
}

func (l *limitedReadCloser) Read(p []byte) (int, error) { return l.r.Read(p) }
func (l *limitedReadCloser) Close() error               { return l.c.Close() }
