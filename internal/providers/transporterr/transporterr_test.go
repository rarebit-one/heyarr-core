package transporterr

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// dialing wraps a connect-time errno the way the net package does on a real
// dial: an *os.SyscallError inside a *net.OpError. Classification must see
// through both layers, because that is the shape the errno actually arrives in.
func dialing(errno syscall.Errno) error {
	return &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9117},
		Err:  os.NewSyscallError("connect", errno),
	}
}

// timeoutError is a net.Error that reports itself timed out without being a
// context deadline — an i/o timeout on the connection, the other way "slow"
// reaches this code.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }

// TestClassify proves each transport failure MODE maps to its own actionable
// detail, so a firewall denial is no longer indistinguishable from a service
// that is down (#242). The blanket "unreachable" survives only as the fallback
// for a genuinely unclassifiable error.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil is the fallback", nil, "unreachable"},
		{"context deadline", context.DeadlineExceeded, "timed out"},
		{"os deadline", os.ErrDeadlineExceeded, "timed out"},
		{"net timeout that is not a context deadline", timeoutError{}, "timed out"},
		{"connection refused — the service is down", dialing(syscall.ECONNREFUSED), "connection refused"},
		{"EPERM — a local firewall or sandbox denied the connect", dialing(syscall.EPERM), "blocked by a local firewall or sandbox policy"},
		{"EACCES is the same class as EPERM", dialing(syscall.EACCES), "blocked by a local firewall or sandbox policy"},
		{"no route to host", dialing(syscall.EHOSTUNREACH), "no route to the host"},
		{"network unreachable is the same class", dialing(syscall.ENETUNREACH), "no route to the host"},
		{"connection reset", dialing(syscall.ECONNRESET), "the connection was reset"},
		{"DNS resolution failure", &net.DNSError{Err: "no such host", Name: "indexer.invalid", IsNotFound: true}, "the endpoint's host name did not resolve"},
		{"TLS verification failure", &tls.CertificateVerificationError{Err: errors.New("x509: certificate signed by unknown authority")}, "the server's TLS certificate could not be verified"},
		{"an error nothing recognises stays unreachable", errors.New("something else entirely"), "unreachable"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifySeesThroughWrapping proves the classification reads the error's
// TYPE, not its text: even when an http.Client-style wrapper has prepended the
// request URL — which carries the API key — the errno underneath is still
// found, and the credential never reaches the output.
func TestClassifySeesThroughWrapping(t *testing.T) {
	const secret = "SUPERSECRETKEY"
	// The exact shape http.Client produces: `Get "URL": <transport error>`,
	// where the URL carries apikey=. This is the error #242 must never quote.
	wrapped := fmt.Errorf("Get %q: %w",
		"https://indexer.example/api?t=caps&apikey="+secret,
		dialing(syscall.ECONNREFUSED))

	got := Classify(wrapped)
	if got != "connection refused" {
		t.Errorf("classification did not see through the URL wrapper: got %q, want %q", got, "connection refused")
	}
	if strings.Contains(got, secret) {
		t.Fatalf("the API key leaked into a health detail: %q", got)
	}
}
