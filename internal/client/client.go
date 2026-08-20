// Package client is the HTTP client the `heyarr` CLI uses to talk to a running
// controller (spec §77, ADR-0011).
//
// # It is a client of the API, not a second implementation of it
//
// Everything here goes over /api/v1 exactly as an external integration would.
// The CLI deliberately does not reach into the database for anything the API
// can answer: a `heyarr works list` that read SQLite directly would work on the
// host and nowhere else, and would keep working after the API broke. The three
// commands that *do* touch the database — token, fsck, gc — are host
// administration and say why in their own files.
//
// # Three properties are load-bearing, and each is here rather than in the CLI
//
//   - The default transport is the unix socket. TCP is the override, because
//     the socket needs no credential handling at the network layer and cannot
//     be reached from another host at all.
//   - Errors are RFC 9457 problem documents and are rendered as their detail.
//     A CLI that prints "request failed: 404" has thrown away the half of the
//     response a human needed: the server said "no work with that identifier".
//   - Collections are keyset-paginated with an opaque cursor, and List follows
//     the cursors to exhaustion. A `list` that fetches one page and stops is
//     wrong for a library of 40 000 works, and wrong silently — it returns 50
//     rows and no indication that there are 39 950 more.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// APIPrefix is where the JSON API is mounted. It is repeated here rather than
// imported from the server package so that the client does not drag chi,
// Prometheus and SQLite into every binary that wants to call Heyarr.
const APIPrefix = "/api/v1"

// unixHost is the Host header used for a unix-socket connection. The value is
// arbitrary — the dialler ignores it — but it must be a valid host, and it
// shows up in the server's access log, so it says what it is.
const unixHost = "unix"

// DefaultTimeout bounds an ordinary request. Streaming calls (blob content and
// the event stream) are exempt: a 20 GB range read and an idle event stream are
// both legitimately longer than any timeout worth setting.
const DefaultTimeout = 30 * time.Second

// Options configure a Client.
type Options struct {
	// Addr is where the API is. See ParseAddr for the accepted forms. Empty
	// means UnixSocket.
	Addr string
	// UnixSocket is the default transport, used when Addr is empty.
	UnixSocket string
	// Token is the bearer credential. Empty is legitimate: authentication can
	// be disabled on a loopback-only deployment (ADR-0011).
	Token string
	// UserAgent identifies the caller in the server's access log.
	UserAgent string
	// Timeout bounds non-streaming requests. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Client calls the Heyarr API.
type Client struct {
	base   string
	target target
	token  string
	agent  string
	httpc  *http.Client
	// stream is the same transport with no timeout, for SSE and blob content.
	stream *http.Client
}

// target describes where and how to connect, kept so that error messages can
// name the socket path rather than the fabricated http://unix URL.
type target struct {
	// scheme is "unix" or "tcp".
	scheme string
	// address is the socket path or the host:port.
	address string
}

// String renders the target the way an operator wrote it.
func (t target) String() string {
	if t.scheme == "unix" {
		return "unix://" + t.address
	}
	return t.address
}

// New builds a Client. It binds nothing and dials nothing — a connection
// failure is reported by the first call, with the address in it.
func New(opts Options) (*Client, error) {
	tgt, err := ParseAddr(opts.Addr, opts.UnixSocket)
	if err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	agent := opts.UserAgent
	if agent == "" {
		agent = "heyarr-cli"
	}

	transport := &http.Transport{
		// The idle-connection settings are the standard library's; only the
		// dialler differs, and only for the unix case.
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	base := "http://" + tgt.address
	if tgt.scheme == "unix" {
		socket := tgt.address
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
		base = "http://" + unixHost
	} else if strings.HasPrefix(opts.Addr, "https://") {
		base = "https://" + tgt.address
	}

	return &Client{
		base:   base,
		target: tgt,
		token:  opts.Token,
		agent:  agent,
		httpc:  &http.Client{Transport: transport, Timeout: timeout},
		// No timeout. A range read of a 20 GB remux (ADR-0013) and an event
		// stream that is idle for an hour are both correct, and a client-side
		// deadline would turn each into a truncated file or a lost stream.
		stream: &http.Client{Transport: transport},
	}, nil
}

// ParseAddr resolves where to connect.
//
// The accepted forms, in the order they are recognised:
//
//	(empty)                   the unix socket, which is the default transport
//	unix:///var/lib/x.sock    an explicit unix socket
//	/var/lib/x.sock           an absolute path is a socket
//	http://127.0.0.1:7777     an explicit TCP endpoint
//	127.0.0.1:7777            a bare host:port is http://
//
// A scheme that is not http, https or unix is rejected rather than guessed at:
// "ftp://host" almost certainly means the operator has the wrong idea about
// something, and connecting anyway would hide it.
func ParseAddr(addr, unixSocket string) (target, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		if unixSocket == "" {
			return target{}, errors.New(
				"no API address: http.unix_socket is empty and --addr was not given — " +
					"pass --addr http://127.0.0.1:7777 or set http.unix_socket")
		}
		return target{scheme: "unix", address: unixSocket}, nil
	}
	switch {
	case strings.HasPrefix(addr, "unix://"):
		path := strings.TrimPrefix(addr, "unix://")
		if path == "" {
			return target{}, fmt.Errorf("--addr %q has no socket path", addr)
		}
		return target{scheme: "unix", address: path}, nil
	case strings.HasPrefix(addr, "/"), strings.HasPrefix(addr, "./"):
		return target{scheme: "unix", address: addr}, nil
	case strings.HasPrefix(addr, "http://"), strings.HasPrefix(addr, "https://"):
		u, err := url.Parse(addr)
		if err != nil || u.Host == "" {
			return target{}, fmt.Errorf("--addr %q is not a usable URL", addr)
		}
		return target{scheme: "tcp", address: u.Host}, nil
	case strings.Contains(addr, "://"):
		return target{}, fmt.Errorf("--addr %q uses a scheme this client cannot speak; "+
			"use http://, https:// or unix://", addr)
	default:
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return target{}, fmt.Errorf("--addr %q is not host:port, a URL or a socket path", addr)
		}
		return target{scheme: "tcp", address: addr}, nil
	}
}

// Target renders where this client connects, for messages and for --json
// output that records what was talked to.
func (c *Client) Target() string { return c.target.String() }

// newRequest builds a request against the API prefix.
func (c *Client) newRequest(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Request, error) {
	u := c.base + APIPrefix + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.agent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// do issues a request and returns the response with a usable status, or an
// error that already reads as a sentence.
func (c *Client) do(httpc *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, c.dialError(err)
	}
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		return nil, c.problemError(req, resp)
	}
	return resp, nil
}

// dialError turns a transport failure into something an operator can act on.
// "connection refused" alone does not say what was not listening.
func (c *Client) dialError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	hint := "is heyarr running?"
	if c.target.scheme == "unix" {
		hint = "is heyarr running, and is this the right data directory?"
	}
	return fmt.Errorf("cannot reach the Heyarr API at %s — %s: %w", c.target, hint, err)
}

// Error is a failed API call. It carries the problem document so a caller can
// branch on the stable `type` URI rather than on prose, and renders as the
// detail so a human sees what the server actually said.
type Error struct {
	Method  string
	Path    string
	Status  int
	Problem *problem.Problem
	// Body is the raw response when it was not a problem document at all —
	// which happens when something in front of Heyarr answered instead.
	Body string
}

// Error renders the server's own explanation.
func (e *Error) Error() string {
	if e.Problem != nil && e.Problem.Detail != "" {
		return e.Problem.Detail
	}
	if e.Problem != nil && e.Problem.Title != "" {
		return e.Problem.Title
	}
	body := strings.TrimSpace(e.Body)
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	if body != "" {
		return fmt.Sprintf("%s %s: the server answered %d with %s",
			e.Method, e.Path, e.Status, body)
	}
	return fmt.Sprintf("%s %s: the server answered %d", e.Method, e.Path, e.Status)
}

// Type is the stable problem type URI, or "" when the response was not a
// problem document. Branch on this, never on the prose.
func (e *Error) Type() string {
	if e.Problem == nil {
		return ""
	}
	return e.Problem.Type
}

// IsNotFound reports whether err is a 404 from the API.
func IsNotFound(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// problemError decodes an error response.
//
// The status is kept alongside the document, because a response that is not a
// problem document at all — an nginx error page, a captive portal — still has
// to produce a sentence rather than a decoding failure.
func (c *Client) problemError(req *http.Request, resp *http.Response) error {
	// Bounded: an error body is small, and an unbounded read here would let a
	// misconfigured intermediary hand the CLI an arbitrary amount of HTML.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := &Error{
		Method: req.Method,
		Path:   req.URL.Path,
		Status: resp.StatusCode,
		Body:   string(raw),
	}
	mediaType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(mediaType, problem.MediaType) || strings.HasPrefix(mediaType, "application/json") {
		var p problem.Problem
		if err := json.Unmarshal(raw, &p); err == nil && (p.Title != "" || p.Detail != "") {
			out.Problem = &p
		}
	}
	return out
}

// Get decodes a GET into out. A nil out discards the body.
func (c *Client) Get(ctx context.Context, path string, q url.Values, out any) error {
	req, err := c.newRequest(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	return c.roundTrip(req, out)
}

// Post sends a JSON body and decodes the response into out.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, nil, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.roundTrip(req, out)
}

// Delete issues a DELETE. The API answers 204, so there is nothing to decode.
func (c *Client) Delete(ctx context.Context, path string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		return err
	}
	return c.roundTrip(req, nil)
}

func (c *Client) roundTrip(req *http.Request, out any) error {
	resp, err := c.do(c.httpc, req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s returned a body this client could not decode: %w",
			req.Method, req.URL.Path, err)
	}
	return nil
}
