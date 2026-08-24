package controller

import (
	"net"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/config"
)

// renderBaseURL is the absolute origin a renderer should fetch bytes from
// (ADR-0040), or empty when this node cannot name one.
//
// A television never spoke to the controller, so it has no notion of "the host
// you got the plan from" and a relative URL is useless to it. Everything here
// is about refusing to guess: an origin that is merely plausible produces a
// device that fails to fetch, which looks to the household like Heyarr being
// broken rather than like a missing setting.
//
// Empty is a supported answer. POST /playback still works, still returns
// ContentURL and a token, and simply carries no renderer URL — the same shape
// as a node with no signing secret.
func renderBaseURL(cfg config.Config) string {
	// A configured peer endpoint is the operator's own statement of how this
	// node is reached, which beats anything derivable. It is skipped only when
	// it names a transport a renderer cannot dial.
	if endpoint := strings.TrimSpace(cfg.Peer.Endpoint); endpoint != "" {
		if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
			return strings.TrimRight(endpoint, "/")
		}
		// unix:// is the common case here, and it is exactly the one to
		// decline: the socket is how the CLI and the workers reach this
		// process, and no device on the network can open it.
		return ""
	}

	host, port, err := net.SplitHostPort(cfg.HTTP.Addr)
	if err != nil || port == "" || port == "0" {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "[::]", "::":
		// A wildcard bind is reachable at many addresses and names none of
		// them, which is the same reason config.PeerEndpoint refuses to guess.
		// Picking one interface here would be right on a single-homed host and
		// wrong the moment there are two, and the failure would be a
		// television silently fetching from the wrong network.
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		// Loopback is not an address another device can use. A node bound only
		// to loopback has deliberately made itself unreachable, and minting a
		// URL pointing at 127.0.0.1 would hand every renderer a link to
		// itself.
		return ""
	}
	return "http://" + net.JoinHostPort(host, port)
}
