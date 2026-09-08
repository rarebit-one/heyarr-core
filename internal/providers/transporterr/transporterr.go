// Package transporterr classifies a transport-level failure into a short,
// actionable health detail WITHOUT reproducing the error's text.
//
// A transport error's Error() can contain the request URL, and the request URL
// carries the API key — so the text can never reach a health detail that
// GET /api/v1/providers returns, an endpoint whose contract promises no
// credential is ever returned. The failure MODE, though, is carried by the
// error's TYPE rather than its text, so naming it discloses nothing: every
// string this package returns is a fixed constant chosen here, and the URL
// never enters the output.
//
// It lives beside the provider interface rather than inside it because
// classifying an error means importing net, crypto/tls and syscall — the very
// transport packages the providers package forbids itself (ADR-0026). A
// provider IMPLEMENTATION already imports those; this is shared logic for the
// implementations, not for the value-typed interface.
package transporterr

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"syscall"
)

// Classify turns a transport failure into an actionable health detail.
//
// "unreachable" used to be the only answer, which made a firewall denial
// (EPERM/EACCES on connect) indistinguishable from a service that is down
// (ECONNREFUSED), one that is slow (a timeout), a name that does not resolve
// (DNS), or a broken route — every one of which sends an operator somewhere
// different. It stays the honest fallback for a failure that genuinely cannot
// be classified, which is all it should ever have meant (#242).
func Classify(err error) string {
	if err == nil {
		return "unreachable"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, os.ErrDeadlineExceeded):
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
		return "blocked by a local firewall or sandbox policy"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "no route to the host"
	case errors.Is(err, syscall.ECONNRESET):
		return "the connection was reset"
	}
	// DNS is a distinct type rather than an errno: name it before any generic
	// timeout branch so a name that will not resolve is not misfiled as slow.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "the endpoint's host name did not resolve"
	}
	// A TLS handshake that fails verification is a configuration problem the
	// operator fixes at the endpoint, not a network one.
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "the server's TLS certificate could not be verified"
	}
	// A transport-level timeout that did not arrive as context.DeadlineExceeded
	// (an i/o timeout on the connection, say) still reads as slow, not down.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out"
	}
	return "unreachable"
}
