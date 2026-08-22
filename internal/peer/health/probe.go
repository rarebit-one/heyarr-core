package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultProbeTimeout bounds one probe.
//
// Five seconds is a long time for a health endpoint that touches nothing and a
// short time against a fifteen-minute window: a peer that cannot return a
// static 200 in five seconds is not one to route a read to, and even so it gets
// several more chances before the window closes. It is short enough that a
// sweep over a handful of unreachable peers cannot outlast the beat that
// started it.
const DefaultProbeTimeout = 5 * time.Second

// HTTPProber probes a peer by asking its unauthenticated /healthz.
//
// /healthz rather than /readyz or an API route, for two reasons. It needs no
// token, so a probe cannot start failing because a credential rotated — which
// would be an authentication problem reported as an outage. And it is the
// cheapest thing the peer serves: no database, no CAS, no work. The probe is
// asking whether a process is answering, not whether it is well.
//
// Its status code is IGNORED, deliberately. See Prober, and see the package
// doc: any answer proves reachability, and reading the status here is the
// change that makes a busy peer indistinguishable from a dead one.
type HTTPProber struct {
	// Timeout bounds one probe. Zero means DefaultProbeTimeout.
	Timeout time.Duration
	// Client is used for http:// and https:// endpoints. Nil means a client
	// built with Timeout. A unix:// endpoint always gets its own client,
	// because its transport is bound to one socket path.
	Client *http.Client
}

// ErrNoEndpoint is a peer with nowhere to probe. It is not an outage: a peer
// enrolled by key with no endpoint yet has simply never been given an address,
// and reporting it as unreachable would be a configuration gap wearing an
// outage's clothes. Sweep skips such a peer rather than probing it.
var ErrNoEndpoint = errors.New("health: the peer has no endpoint to probe")

// Probe returns nil if anything answered, whatever it answered.
func (p HTTPProber) Probe(ctx context.Context, peer Peer) error {
	if peer.Endpoint == "" {
		return ErrNoEndpoint
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	client, target, err := p.clientFor(peer.Endpoint, timeout)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("health: building a probe for %s: %w", peer.Endpoint, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Silence. This is the ONLY thing that makes a peer unreachable, and
		// it does so by not calling Answered rather than by recording a
		// failure anywhere.
		return fmt.Errorf("health: probing %s: %w", peer.Endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The body is drained so the connection can be reused, and capped so a peer
	// answering /healthz with a gigabyte cannot turn a probe into a transfer.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	// resp.StatusCode is not examined, here or anywhere. A 500 from a peer
	// mid-scan is a peer that is up.
	return nil
}

// clientFor resolves an endpoint to a client and a URL.
//
// unix:// is handled because it is what a single-host deployment derives for
// itself (config.PeerEndpoint), and a prober that could not speak it would
// report the most common development topology as permanently unreachable.
func (p HTTPProber) clientFor(endpoint string, timeout time.Duration) (*http.Client, string, error) {
	if socket, ok := strings.CutPrefix(endpoint, "unix://"); ok {
		client := &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		}
		// The host is a placeholder: the transport dials the socket and never
		// resolves it.
		return client, "http://heyarr/healthz", nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, "", fmt.Errorf("health: peer endpoint %q is not a URL: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("health: peer endpoint %q has scheme %q; probing speaks http, https and unix", endpoint, u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/healthz"
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return client, u.String(), nil
}
