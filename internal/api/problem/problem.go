// Package problem renders API errors as RFC 9457 problem details.
//
// Every error the HTTP API returns is a problem document, and every problem
// document carries a stable `type` URI. That URI — not the title, not the
// detail, not the HTTP status alone — is the contract: a client branches on
// `type`, so the prose can be reworded, translated or made more specific
// without breaking anything. Titles are for humans reading a log; details are
// for humans reading a terminal.
//
// There is deliberately no second machine-readable field. A `type` URI *and* a
// short `code` means two identifiers that can disagree, and the one clients
// pick is whichever they noticed first.
package problem

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// MediaType is the content type of a problem document (RFC 9457 §3).
const MediaType = "application/problem+json"

// TypeBase prefixes every problem type URI. The URI is an identifier, not a
// promise that something is published there — but keeping it dereferenceable
// costs nothing and documentation can appear later without a wire change.
const TypeBase = "https://heyarr.dev/problems/"

// The stable type URIs. Adding one is additive; changing one is a breaking API
// change and needs a deprecation, exactly like renaming a field.
const (
	TypeBadRequest   = TypeBase + "bad-request"
	TypeUnauthorized = TypeBase + "unauthorized"
	TypeForbidden    = TypeBase + "forbidden"
	TypeNotFound     = TypeBase + "not-found"
	TypeConflict     = TypeBase + "conflict"
	TypeInternal     = TypeBase + "internal"
)

// Problem is an RFC 9457 problem document.
//
// RequestID is an extension member. It is here because the first question
// asked about any 500 is "which request?", and an operator who has to correlate
// by timestamp across a busy log will not bother.
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// Error lets a Problem be returned as an ordinary error and rendered later.
func (p *Problem) Error() string {
	if p.Detail != "" {
		return fmt.Sprintf("%s: %s", p.Title, p.Detail)
	}
	return p.Title
}

// New builds a problem with an explicit type URI and status. Prefer the named
// constructors; this exists for the cases they do not cover.
func New(status int, typeURI, title, detail string) *Problem {
	return &Problem{Type: typeURI, Title: title, Status: status, Detail: detail}
}

// BadRequest reports a malformed or semantically invalid request (400).
func BadRequest(detail string) *Problem {
	return New(http.StatusBadRequest, TypeBadRequest, "Bad Request", detail)
}

// Unauthorized reports missing or unusable credentials (401).
func Unauthorized(detail string) *Problem {
	return New(http.StatusUnauthorized, TypeUnauthorized, "Unauthorized", detail)
}

// Forbidden reports credentials that are valid but insufficient (403).
func Forbidden(detail string) *Problem {
	return New(http.StatusForbidden, TypeForbidden, "Forbidden", detail)
}

// NotFound reports an absent resource (404).
func NotFound(detail string) *Problem {
	return New(http.StatusNotFound, TypeNotFound, "Not Found", detail)
}

// Conflict reports a request that contradicts current state (409).
func Conflict(detail string) *Problem {
	return New(http.StatusConflict, TypeConflict, "Conflict", detail)
}

// Internal reports a server-side failure (500).
//
// The detail is deliberately fixed. Internal errors are where implementation
// detail leaks — a file path, a SQL string, a stack — and the client is never
// the right audience for any of it. The log is.
func Internal() *Problem {
	return New(http.StatusInternalServerError, TypeInternal, "Internal Server Error",
		"the server failed to handle this request; see the server log for the request id")
}

// WithInstance records the URI this problem occurred at.
func (p *Problem) WithInstance(uri string) *Problem { p.Instance = uri; return p }

// WithRequestID records the correlating request id.
func (p *Problem) WithRequestID(id string) *Problem { p.RequestID = id; return p }

// Write renders p to w, filling Instance from r when it is not already set.
//
// Callers inside the HTTP server should use httpapi.Fail instead, which also
// stamps the request id. This package does not reach for it itself: problem
// documents are the lower layer and must not import the server.
//
// Caching is disabled explicitly: a cache that stores a 404 or a 401 turns a
// transient condition into a sticky one, and intermediaries are entitled to do
// exactly that unless told otherwise.
func Write(w http.ResponseWriter, r *http.Request, p *Problem) {
	if p == nil {
		p = Internal()
	}
	if p.Instance == "" && r != nil {
		p.Instance = r.URL.Path
	}
	body, err := json.Marshal(p)
	if err != nil {
		// Problem is a fixed struct of strings and an int; marshalling it
		// cannot fail. If it somehow does, an empty 500 beats a panic.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", MediaType)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}
