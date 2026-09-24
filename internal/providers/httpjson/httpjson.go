// Package httpjson is the one JSON-over-HTTP round trip the metadata and
// subtitle provider adapters share: an optional rate-limit wait, a request with
// Accept: application/json (plus the adapter's User-Agent and credential
// headers), a body read under a size cap, a non-200 turned into an *Error that
// carries the status, and a JSON decode.
//
// Every error is prefixed with the adapter's service name and the operation, so
// a failed discovery, enumeration or health check are told apart in a health
// detail or a log. Credentials travel only in headers the caller passes, never
// in the URL, so an error that renders the request URL cannot leak them.
//
// Like transporterr, it lives beside the provider interface rather than inside
// it: it imports net/http, which the providers package forbids itself
// (ADR-0026). It is shared logic for the implementations.
package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/rarebit-one/heyarr-core/internal/providers/ratelimit"
)

// Client is one adapter's view of the round trip. It is a value an adapter
// builds per call from its own fields, so a test that swaps the adapter's
// *http.Client is honoured.
type Client struct {
	// HTTP sends the request.
	HTTP *http.Client
	// Service prefixes every error: "tmdb: search returned HTTP 401".
	Service string
	// MaxBody caps how much of a response body is read.
	MaxBody int64
	// UserAgent is sent when non-empty; an adapter whose service requires a
	// descriptive one (MusicBrainz, Open Library, OpenSubtitles) sets it.
	UserAgent string
	// Limiter, when set, is waited on before every request is built. Its error
	// (a cancelled context) is returned as is.
	Limiter *ratelimit.RateLimiter
}

// Header is one extra request header — in practice a credential.
type Header struct{ Key, Value string }

// Bearer is an Authorization: Bearer header.
func Bearer(token string) Header { return Header{Key: "Authorization", Value: "Bearer " + token} }

// Error is a non-200 answer, carrying the status so a caller (and a health
// Check) can tell an auth failure from a throttle or an outage.
type Error struct {
	Service string
	Op      string
	Status  int
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s returned HTTP %d", e.Service, e.Op, e.Status)
}

// Get performs a GET of url and decodes the JSON body into into.
func (c Client) Get(ctx context.Context, url, op string, into any, headers ...Header) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%s: building %s request: %w", c.Service, op, err)
	}
	return c.do(req, op, into, headers)
}

// Post performs a POST of a JSON body to url and decodes the JSON answer into
// into.
func (c Client) Post(ctx context.Context, url, op string, body []byte, into any, headers ...Header) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: building %s request: %w", c.Service, op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, op, into, headers)
}

func (c Client) wait(ctx context.Context) error {
	if c.Limiter == nil {
		return nil
	}
	return c.Limiter.Wait(ctx)
}

// do sets the shared headers, sends the request, reads the bounded body and
// decodes it, mapping a non-200 to an *Error.
func (c Client) do(req *http.Request, op string, into any, headers []Header) error {
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	for _, h := range headers {
		req.Header.Set(h.Key, h.Value)
	}

	//nolint:gosec // G704: the URL is the adapter's own configured endpoint plus a path it built; this is the provider round trip
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %s request failed: %w", c.Service, op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxBody))
	if err != nil {
		return fmt.Errorf("%s: reading %s response: %w", c.Service, op, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &Error{Service: c.Service, Op: op, Status: resp.StatusCode}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: decoding %s response: %w", c.Service, op, err)
	}
	return nil
}
