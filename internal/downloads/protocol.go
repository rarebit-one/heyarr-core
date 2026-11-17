package downloads

import (
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Each client says which protocol it fetches, so a grab offers it only
// releases of that protocol (#836). Gathered here rather than beside each
// client so the whole routing table reads at once. A client added without a
// line here is offered every release, as every client was before.
//
// The direct clients each accept only their own source shape (an http(s)
// URL, a store's scheme, a capture or yt-dlp tag), so within the direct
// protocol the first acceptor is still the right one.

var (
	_ providers.ProtocolDownloader = (*Client)(nil)
	_ providers.ProtocolDownloader = (*QBClient)(nil)
	_ providers.ProtocolDownloader = (*SABClient)(nil)
	_ providers.ProtocolDownloader = (*NZBGetClient)(nil)
	_ providers.ProtocolDownloader = (*HTTPClient)(nil)
	_ providers.ProtocolDownloader = (*GOGClient)(nil)
	_ providers.ProtocolDownloader = (*WebCaptureClient)(nil)
	_ providers.ProtocolDownloader = (*YtDlpClient)(nil)

	_ providers.HTTPFetcher = (*HTTPClient)(nil)
)

// Protocol implements providers.ProtocolDownloader: Transmission fetches torrents.
func (c *Client) Protocol() acquisition.Protocol { return acquisition.ProtocolTorrent }

// Protocol implements providers.ProtocolDownloader: qBittorrent fetches torrents.
func (c *QBClient) Protocol() acquisition.Protocol { return acquisition.ProtocolTorrent }

// Protocol implements providers.ProtocolDownloader: SABnzbd fetches usenet.
func (c *SABClient) Protocol() acquisition.Protocol { return acquisition.ProtocolUsenet }

// Protocol implements providers.ProtocolDownloader: NZBGet fetches usenet.
func (c *NZBGetClient) Protocol() acquisition.Protocol { return acquisition.ProtocolUsenet }

// Protocol implements providers.ProtocolDownloader: the http client fetches
// the URL as the payload.
func (c *HTTPClient) Protocol() acquisition.Protocol { return acquisition.ProtocolDirect }

// FetchesHTTP implements providers.HTTPFetcher: of the direct clients, only
// this one takes a plain http(s) URL.
func (c *HTTPClient) FetchesHTTP() bool { return true }

// Protocol implements providers.ProtocolDownloader: a store's package is
// fetched from the store itself.
func (c *GOGClient) Protocol() acquisition.Protocol { return acquisition.ProtocolDirect }

// Protocol implements providers.ProtocolDownloader: a captured page is
// fetched as itself.
func (c *WebCaptureClient) Protocol() acquisition.Protocol { return acquisition.ProtocolDirect }

// Protocol implements providers.ProtocolDownloader: a video yt-dlp fetches
// is fetched as itself.
func (c *YtDlpClient) Protocol() acquisition.Protocol { return acquisition.ProtocolDirect }
