package controller

import (
	"context"
	"net"
	"strconv"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/discovery"
)

// startDiscovery builds the mDNS advertiser and starts it unless the node has
// nothing worth announcing (ADR-0094 §Discovery, Phase 2).
//
// It always returns a non-nil Advertiser so the caller can Stop() it
// unconditionally on shutdown: an advertiser that was never started, or was
// suppressed, treats Stop as a no-op. Advertisement is skipped — the advertiser
// returned inert — when the node names no LAN-reachable port (socket-only or
// loopback-only), or when the operator disabled it (http.discovery.disabled).
// Beyond that, the advertiser itself is inert when no trusted, multicast-capable
// interface exists: the guest trust boundary (http.guest.trusted_nets) is the
// same allow-list, so an empty boundary announces to nobody.
func (c *Controller) startDiscovery(ctx context.Context, boundAddr string) *discovery.Advertiser {
	nets, err := c.cfg.HTTP.Guest.ParsedNets()
	if err != nil {
		// Validated at config load, so this cannot fire; logged rather than
		// dropped so a future change cannot pass an unparsed value through
		// silently, and returned inert so Stop stays safe.
		c.log.Warn("discovery: could not parse the trusted nets; not advertising", "error", err)
		return discovery.New(discovery.Options{})
	}

	port, ok := advertisePort(boundAddr)
	adv := discovery.New(discovery.Options{
		Params: discovery.Params{
			Port:    port,
			TLS:     c.cfg.HTTP.TLS.Enabled(),
			APIPath: httpapi.APIPrefix,
		},
		TrustedNets: nets,
		Logger:      c.log,
	})

	if !ok {
		c.log.Info("mDNS advertisement disabled: the node names no LAN-reachable port",
			"http_addr", boundAddr)
		return adv
	}
	if !c.cfg.HTTP.Discovery.Advertises() {
		c.log.Info("mDNS advertisement disabled by configuration (http.discovery.disabled)")
		return adv
	}
	if _, err := adv.Start(ctx); err != nil {
		// A failure to open the multicast socket is not fatal to the node: the
		// API is up and reachable by the DNS name and manual entry, so discovery
		// degrades rather than taking the controller down with it.
		c.log.Warn("mDNS advertisement failed to start", "error", err)
	}
	return adv
}

// advertisePort reports the TCP port to advertise and whether the node is
// reachable off-host at all. It is the discovery analog of renderBaseURL's
// "empty is a supported answer": a node with no TCP listener, or one bound only
// to loopback, names no port a device on the LAN could dial, so it advertises
// none. A wildcard bind (0.0.0.0 / ::) IS reachable — it listens on every
// interface — and the per-interface A/AAAA records carry the address a client
// actually uses, so the bound port alone is what discovery needs from here.
func advertisePort(boundAddr string) (uint16, bool) {
	if boundAddr == "" {
		return 0, false // a socket-only node
	}
	host, portStr, err := net.SplitHostPort(boundAddr)
	if err != nil {
		return 0, false
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p > 65535 {
		return 0, false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return 0, false
	}
	return uint16(p), true
}
