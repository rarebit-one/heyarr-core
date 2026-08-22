// Package endpoint validates and normalises where a peer is reachable.
//
// It exists because "where to reach a peer" was a free string until #169: the
// control plane accepted whatever an operator typed, stored it, echoed it back
// in a table, and then produced a raw net/url parse error at the moment
// something first dialled it — naming a path segment the operator never typed,
// long after the command that recorded the value had exited successfully.
//
// The endpoint is therefore checked where it is WRITTEN, the way config.Peer's
// listen address is checked at startup rather than at bind. That matters more
// here than it looks: `peers add` is idempotent on the public key, so a
// re-registration with a typo'd endpoint silently replaces a working one, and
// the peer stays enrolled and looks healthy in `peers list` while being
// unreachable.
//
// A bare host:port is normalised rather than refused, and it is normalised to
// https. Guessing a scheme is usually a mistake; here there is exactly one
// answer, because the inter-peer transport is mutually authenticated TLS pinned
// to membership (ADR-0012).
//
// http:// is refused for the same reason, and that refusal is the point rather
// than an inconvenience: a peer endpoint an operator writes is a network hop to
// another site. unix:// is accepted because it is a local socket rather than a
// network hop — it is what a single-host deployment derives for itself
// (config.PeerEndpoint), and internal/peer/health probes it.
//
// This is the OPERATOR boundary's rule: `peers add` and POST /api/v1/peers
// apply it, which is #169's whole argument — the value is checked where it is
// written, the way config.Peer.Listen is checked at startup. The membership
// store deliberately does not, so that a test or an internal caller
// constructing membership is not made to satisfy a rule about what an operator
// may type.
package endpoint

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ErrMalformed is the sentinel every refusal here wraps, so a caller can map
// it to a status or a flag name without matching on message text.
var ErrMalformed = errors.New("peer endpoint is not usable")

// Scheme is the only scheme the inter-peer path speaks, and what a bare
// host:port is completed with (ADR-0012).
const Scheme = "https"

// Example is shown in every refusal. An error that says a value is wrong and
// not what a right one looks like leaves the operator guessing a second time.
const Example = Scheme + "://192.168.1.10:8443"

// Normalise returns the endpoint to store, or explains why the value cannot be
// one. The empty string is refused rather than normalised: a caller for whom
// "no endpoint" is legitimate — a peer enrolled before anyone knows where it
// will live — decides that before calling, and an empty value that reached
// here was typed.
func Normalise(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("%w: it is empty; give an address like %s", ErrMalformed, Example)
	}
	if socket, ok := strings.CutPrefix(value, "unix://"); ok {
		return normaliseSocket(value, socket)
	}
	if strings.Contains(value, "://") {
		return normaliseURL(value)
	}
	return normaliseHostPort(value)
}

// normaliseSocket accepts the endpoint a single-host deployment derives for
// itself (config.PeerEndpoint). It is kept verbatim: a socket path is a path,
// and there is nothing about it to normalise beyond insisting it exists.
func normaliseSocket(value, socket string) (string, error) {
	if socket == "" {
		return "", fmt.Errorf("%w: %q has no socket path after unix://; give an address like %s",
			ErrMalformed, value, Example)
	}
	if !strings.HasPrefix(socket, "/") {
		return "", fmt.Errorf("%w: %q must name an absolute socket path, and %q is relative — "+
			"a relative path resolves against whichever directory the reader happens to be in "+
			"(or give an address like %s)",
			ErrMalformed, value, socket, Example)
	}
	return value, nil
}

// normaliseURL accepts a scheme-qualified endpoint.
func normaliseURL(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("%w: %q is not a URL (%w); give an address like %s",
			ErrMalformed, value, err, Example)
	}
	if u.Scheme != Scheme {
		return "", fmt.Errorf("%w: %q uses %q, and the inter-peer path speaks %s only — "+
			"it is mutually authenticated TLS pinned to membership (ADR-0012); give an address like %s",
			ErrMalformed, value, u.Scheme, Scheme, Example)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: %q names no host, so it does not say which machine; "+
			"give an address like %s", ErrMalformed, value, Example)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: %q carries credentials, and a peer authenticates with its key "+
			"rather than a password (ADR-0012); give an address like %s", ErrMalformed, value, Example)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: %q carries a query or fragment, and a peer endpoint is an origin; "+
			"give an address like %s", ErrMalformed, value, Example)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("%w: %q has the path %q, and a peer endpoint is an origin — "+
			"the peer API's own prefix is appended to it; give an address like %s",
			ErrMalformed, value, u.Path, Example)
	}
	if port := u.Port(); port != "" {
		if err := checkPort(value, port); err != nil {
			return "", err
		}
	}
	return Scheme + "://" + u.Host, nil
}

// normaliseHostPort accepts a bare host:port and supplies the only scheme this
// fabric has.
func normaliseHostPort(value string) (string, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("%w: %q is neither a URL nor host:port (%w); "+
			"give an address like %s", ErrMalformed, value, err, Example)
	}
	if host == "" {
		return "", fmt.Errorf("%w: %q gives a port but no host, so it does not say which machine; "+
			"give an address like %s", ErrMalformed, value, Example)
	}
	if err := checkPort(value, port); err != nil {
		return "", err
	}
	return Scheme + "://" + net.JoinHostPort(host, port), nil
}

// checkPort refuses a port that is not a port. net.SplitHostPort splits on the
// last colon and does not look at what follows it, so "host:notaport" arrives
// here as a perfectly well-formed pair.
func checkPort(value, port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%w: %q has %q where a port number between 1 and 65535 belongs; "+
			"give an address like %s", ErrMalformed, value, port, Example)
	}
	return nil
}
