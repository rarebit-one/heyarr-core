package downloads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// The Transmission RPC transport.
//
// # The 409 handshake is the whole reason this file exists
//
// Transmission answers the FIRST request of a session with 409 Conflict and an
// X-Transmission-Session-Id header. The client is expected to replay that
// header on every subsequent request, and to repeat the dance whenever the
// server rotates the id — which it does, without warning, at intervals it does
// not document.
//
// So a 409 is not an error at any point in a session's life. It is the
// protocol's way of saying "use this id now". A client that treats it as a
// failure works against every hand-written fixture — because whoever wrote
// them would have recorded the 200 they got AFTER the handshake — and fails
// against every real instance, immediately and forever.
//
// The captured corpus contains that 409 as its own exchange for exactly this
// reason.

// sessionHeader is the header Transmission issues and expects back.
const sessionHeader = "X-Transmission-Session-Id"

// maxHandshakeRetries bounds how many times one call will re-handshake.
//
// One is the normal case: the id was absent or stale, the server said so, we
// replay. Two exists because the id can rotate BETWEEN our retry and the
// server reading it, which is rare and real. Beyond that a 409 loop means
// something is wrong that retrying will not fix — a proxy stripping the header,
// most likely — and spinning would turn a configuration mistake into load.
const maxHandshakeRetries = 2

// rpcError is a Transmission RPC call that failed in a way the caller may need
// to distinguish.
type rpcError struct {
	// Status is the HTTP status, zero when the failure was below HTTP.
	Status int
	// Detail says what happened, without quoting a credential.
	Detail string
	err    error
}

func (e *rpcError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("transmission: %s (HTTP %d)", e.Detail, e.Status)
	}
	return "transmission: " + e.Detail
}

func (e *rpcError) Unwrap() error { return e.err }

// ErrUnauthorised is what a 401 produces.
//
// A distinct error because it is a CONFIGURATION problem, not a transient one:
// retrying a wrong credential forever produces an unhealthy provider whose
// detail says "unreachable", which sends an operator to look at the network.
//
// ## UNFIXTURED
//
// The captured corpus contains no 401. Obtaining one means enabling
// rpc-authentication on a real instance, and Transmission rewrites
// settings.json on clean shutdown — so that is not a change to make in passing,
// and a synthesised 401 would be a fixture testing that this client agrees with
// whoever invented it (ADR-0026). The path is implemented and unit-tested
// against a hand-built httptest server, which is honest about being a test
// double rather than dressed up as a capture.
var ErrUnauthorised = errors.New("transmission: the credential was refused")

// ErrRPCFailure is a call Transmission understood and declined.
var ErrRPCFailure = errors.New("transmission: the call was declined")

// transport performs authenticated, session-aware RPC calls.
//
// It is not the Downloader — that is client.go. This is only the part that
// knows about HTTP, the session id and the envelope, kept separate so that
// everything above it reads as method calls rather than as request building.
type transport struct {
	endpoint string
	user     string
	pass     string
	http     *http.Client

	// mu guards sessionID. The poll job and a health check can call
	// concurrently under `heyarr all`, and two goroutines racing to
	// re-handshake would each store the other's id.
	mu        sync.Mutex
	sessionID string
}

// envelope is Transmission's request and response shape.
//
// One type for both directions because the protocol uses one: a request is
// {method, arguments, tag} and a response is {result, arguments, tag}. Two
// types would be two places to keep the tag handling consistent.
type envelope struct {
	Method    string          `json:"method,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    string          `json:"result,omitempty"`
}

// call performs one RPC method, decoding arguments into out.
//
// out may be nil for a call whose result is only success or failure.
func (t *transport) call(ctx context.Context, method string, args any, out any) error {
	var raw json.RawMessage
	if args != nil {
		encoded, err := json.Marshal(args)
		if err != nil {
			return &rpcError{Detail: "encoding " + method, err: err}
		}
		raw = encoded
	}
	body, err := json.Marshal(envelope{Method: method, Arguments: raw})
	if err != nil {
		return &rpcError{Detail: "encoding " + method, err: err}
	}

	for attempt := 0; ; attempt++ {
		resp, err := t.do(ctx, body)
		if err != nil {
			return err
		}

		// The handshake. NOT an error — see the file comment.
		if resp.status == http.StatusConflict {
			if attempt >= maxHandshakeRetries {
				return &rpcError{
					Status: resp.status,
					Detail: "the session id was refused repeatedly; " +
						"something between here and Transmission is dropping " +
						sessionHeader,
				}
			}
			if resp.sessionID == "" {
				return &rpcError{
					Status: resp.status,
					Detail: "a 409 carried no " + sessionHeader +
						", so there is no id to replay",
				}
			}
			t.setSession(resp.sessionID)
			continue
		}

		switch {
		case resp.status == http.StatusUnauthorized:
			return ErrUnauthorised
		case resp.status < 200 || resp.status > 299:
			return &rpcError{
				Status: resp.status,
				Detail: "the RPC endpoint answered " + summarise(resp.body),
			}
		}

		var env envelope
		if err := json.Unmarshal(resp.body, &env); err != nil {
			return &rpcError{
				Status: resp.status,
				Detail: "the response is not an RPC envelope: " + summarise(resp.body),
				err:    err,
			}
		}
		if env.Result != "success" {
			// Transmission reports application-level failures in `result`
			// with a 200. Reading only the status would treat "duplicate
			// torrent" as a success.
			return fmt.Errorf("%w: %s", ErrRPCFailure, env.Result)
		}
		if out != nil && len(env.Arguments) > 0 {
			if err := json.Unmarshal(env.Arguments, out); err != nil {
				return &rpcError{Detail: "decoding the " + method + " arguments", err: err}
			}
		}
		return nil
	}
}

// response is one HTTP exchange, reduced to what call needs.
type response struct {
	status    int
	body      []byte
	sessionID string
}

func (t *transport) do(ctx context.Context, body []byte) (response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return response{}, &rpcError{Detail: "building the request", err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if id := t.session(); id != "" {
		req.Header.Set(sessionHeader, id)
	}
	if t.user != "" {
		// Basic auth rather than a credential in the URL: ADR-0025 refuses
		// userinfo in an endpoint precisely so it cannot reach a log line.
		req.SetBasicAuth(t.user, t.pass)
	}

	resp, err := t.http.Do(req)
	if err != nil {
		return response{}, &rpcError{
			// The endpoint is safe to name and the credential is not present
			// in it — configuration refuses userinfo in a URL.
			Detail: "could not reach " + t.endpoint,
			err:    err,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded. An RPC response is a JSON document about torrents; an unbounded
	// read here is a memory-exhaustion primitive handed to whatever is on the
	// other end of a configured URL.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return response{}, &rpcError{Status: resp.StatusCode, Detail: "reading the response", err: err}
	}
	return response{
		status:    resp.StatusCode,
		body:      raw,
		sessionID: resp.Header.Get(sessionHeader),
	}, nil
}

// maxResponseBytes bounds one RPC response.
//
// Generous: torrent-get over a few thousand transfers with trackerStats is
// megabytes. It is a bound on what a misbehaving or hostile endpoint can make
// this process allocate, not a limit anyone meets.
const maxResponseBytes = 64 << 20

func (t *transport) session() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

func (t *transport) setSession(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessionID = id
}

// summarise renders an unexpected body for an error message.
//
// Truncated and single-lined, because the most common non-JSON response is an
// HTML error page from a reverse proxy and pasting one into a log makes the log
// unreadable at precisely the moment somebody is trying to read it.
func summarise(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "an empty body"
	}
	s = strings.Join(strings.Fields(s), " ")
	const limit = 120
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}

// decodeJSON reads a JSON body, used by tests that stand up a server behaving
// the way the daemon does.
func decodeJSON(r *http.Request, out any) error {
	return json.NewDecoder(io.LimitReader(r.Body, maxResponseBytes)).Decode(out)
}
