package gateway

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// Controller says where the gateway proxies library and stream methods, and with
// what credential.
type Controller struct {
	// BaseURL is the controller's origin, without the /rest suffix
	// (e.g. "http://10.0.0.2:7777"). Required.
	BaseURL string
	// User and Bearer are the DEVICE's controller credential. The bearer is sent
	// as the Subsonic password (p=), which the controller-side adapter verifies
	// as a Heyarr bearer token (internal/api/subsonic authenticate). This is the
	// credential the stock app never holds — the device presents it upstream on
	// the app's behalf. Bearer is required.
	User   string
	Bearer string
	// Client is the HTTP client to reach the controller with. Optional: a device
	// on the LAN reaches a TCP controller with the default; a unix-socket
	// controller injects a socket-dialling client. When nil a sane default with a
	// generous streaming timeout is used.
	Client *http.Client
}

// proxy forwards a Subsonic method to the controller's /rest adapter, swapping
// the app's credential for the device's controller bearer.
type proxy struct {
	base   string
	user   string
	bearer string
	client *http.Client
}

func newProxy(c Controller) (*proxy, error) {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		return nil, errors.New("gateway: a controller base URL is required")
	}
	if c.Bearer == "" {
		return nil, errors.New("gateway: a controller bearer credential is required")
	}
	client := c.Client
	if client == nil {
		// No overall timeout: a range read of a large blob (ADR-0013) is a
		// legitimately long response, and a client-side deadline would truncate
		// it. The transport's own dial/handshake timeouts still bound setup.
		client = &http.Client{Transport: &http.Transport{
			MaxIdleConns:        4,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}}
	}
	user := c.User
	if user == "" {
		user = "heyarr-gateway"
	}
	return &proxy{base: base, user: user, bearer: c.Bearer, client: client}, nil
}

// forward proxies one method to the controller and streams the reply back.
//
// The app's own credential (u/p/t/s) is stripped and the device's controller
// credential is substituted; every other query parameter — id, size, offset,
// type, format — is passed through untouched, so the controller sees the request
// the app made, authenticated as the device. The Range request header is
// forwarded so 206 partial reads and M10 progressive serving work through the
// gateway, and the byte-carrying response headers and body are copied back
// verbatim: the gateway adds no byte-path code of its own, which is what keeps a
// proxied stream byte-identical to the controller's own (ADR-0013).
func (p *proxy) forward(w http.ResponseWriter, r *http.Request, method string) {
	q := r.URL.Query()
	// Strip the app's credential; substitute the device's controller credential.
	q.Del("u")
	q.Del("p")
	q.Del("t")
	q.Del("s")
	q.Set("u", p.user)
	q.Set("p", p.bearer)

	target := p.base + Prefix + "/" + method
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}

	// The gateway IS a reverse proxy: the upstream host is fixed at construction
	// (p.base), and only the Subsonic method — already whitelisted by dispatch
	// before forward is reached — and the app's query string pass through. That
	// is the feature, not an SSRF.
	//nolint:gosec // G704: fixed upstream host; the request is a deliberate proxy hop
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		p.gatewayError(w, r, err)
		return
	}
	// Forward the headers a byte-serving proxy must preserve, and nothing that
	// would leak the app's identity or confuse the controller.
	copyHeader(req.Header, r.Header, "Range", "If-Range", "If-None-Match", "If-Modified-Since", "Accept")

	//nolint:gosec // G704: fixed upstream host; the request is a deliberate proxy hop
	resp, err := p.client.Do(req)
	if err != nil {
		p.gatewayError(w, r, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyHeader(w.Header(), resp.Header,
		"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
		"Last-Modified", "ETag", "Cache-Control")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// gatewayError answers a Subsonic error envelope when the controller could not be
// reached at all. A stock app parses the envelope; an empty 502 would read as a
// broken server rather than an unreachable upstream.
func (p *proxy) gatewayError(w http.ResponseWriter, r *http.Request, _ error) {
	format := "xml"
	if strings.ToLower(r.URL.Query().Get("f")) == "json" {
		format = "json"
	}
	e := &response{
		Namespace: namespace, Status: "failed", Version: apiVersion,
		Type: responseType, OpenSubsonic: true,
		Error: &subError{Code: errGeneric, Message: "the controller could not be reached"},
	}
	writeEnvelope(w, format, e)
}

// writeEnvelope renders a response without a Server (the proxy has none to hand).
func writeEnvelope(w http.ResponseWriter, format string, e *response) {
	(&Server{}).write(w, format, e)
}

func copyHeader(dst, src http.Header, names ...string) {
	for _, n := range names {
		if v := src.Values(n); len(v) > 0 {
			dst[http.CanonicalHeaderKey(n)] = append([]string(nil), v...)
		}
	}
}
