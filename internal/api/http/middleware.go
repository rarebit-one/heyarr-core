package httpapi

import (
	"context"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// RequestIDHeader carries the correlation id, inbound and outbound.
const RequestIDHeader = "X-Request-Id"

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyIdentity
	ctxKeyIdentitySlot
)

// identitySlot lets the access log name the caller.
//
// It has to be a slot rather than a plain context value because the middleware
// that resolves the identity runs *inside* the one that logs: authentication
// derives a new context, and the logger is still holding the old request. The
// slot is written once, by the authentication middleware, and read once, after
// the handler returns, on the same goroutine.
type identitySlot struct{ identity *auth.Identity }

// RequestIDFrom returns the correlation id for a request, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// Fail writes an RFC 9457 problem document, stamped with the request id so a
// user reporting an error and an operator reading the log are talking about the
// same request. Handlers mounted on this server should use it rather than
// problem.Write directly.
func Fail(w http.ResponseWriter, r *http.Request, p *problem.Problem) {
	problem.Write(w, r, p.WithRequestID(RequestIDFrom(r.Context())))
}

// maxInboundRequestID bounds what an inbound correlation id may be. Echoing an
// unbounded client-supplied string into every log line is a log-injection and a
// disk-fill primitive at once.
const maxInboundRequestID = 64

// requestIDMiddleware assigns a correlation id, honouring an inbound one when
// it is sane. It is first in the chain so that everything after it — including
// the access log and the panic recovery — can name the request.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(RequestIDHeader))
		if id == "" {
			id = uuid.Must(uuid.NewV7()).String()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

// sanitizeRequestID keeps an inbound id only if it is short and printable ASCII
// with no control characters — otherwise it is discarded and a fresh one is
// minted, because a forged id is a nuisance while a newline in a log line is a
// forged log entry.
func sanitizeRequestID(s string) string {
	if s == "" || len(s) > maxInboundRequestID {
		return ""
	}
	for _, c := range s {
		if c < 0x20 || c > 0x7e {
			return ""
		}
	}
	return s
}

// responseRecorder observes the status and byte count without getting in the
// way of what a handler needs the ResponseWriter to be able to do.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

// wrapResponse returns w as a recorder, reusing one that is already in place.
// Two nested wrappers would each hide the other's optimised paths, and the
// status the outer one saw would be the one the inner one already defaulted.
func wrapResponse(w http.ResponseWriter) *responseRecorder {
	if rec, ok := w.(*responseRecorder); ok {
		return rec
	}
	return &responseRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer, so a handler can
// still set a read deadline or hijack. Without it, wrapping silently removes
// capabilities from every handler downstream.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// ReadFrom preserves the sendfile fast path. http.ServeContent — which ADR-0013
// makes the way blobs are served — hands the body to io.Copy, and losing the
// ReaderFrom here would turn a zero-copy 20 GB range response into a userspace
// byte shuffle.
func (r *responseRecorder) ReadFrom(src io.Reader) (int64, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		r.written += n
		return n, err
	}
	n, err := io.Copy(r.ResponseWriter, src)
	r.written += n
	return n, err
}

// Flush keeps streaming responses streaming.
func (r *responseRecorder) Flush() {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// accessLogMiddleware logs one structured line per request, after it completes.
//
// What is deliberately absent is as important as what is here. No header is
// logged — the Authorization header is a live credential and a log is the most
// widely copied artefact a system produces. The query string is reduced to its
// parameter *names*, because a client that puts a token in a query parameter
// (they do) must not thereby write it into the log. What identifies the caller
// is the principal and the token id, both of which are safe and both of which
// are what an operator actually wants.
func (s *Server) accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := wrapResponse(w)
		start := time.Now()
		slot := &identitySlot{}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyIdentitySlot, slot))
		defer func() {
			attrs := []any{
				"request_id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"route", routePattern(r),
				"status", rec.status,
				"bytes", rec.written,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", remoteHost(r),
			}
			if keys := queryKeys(r); keys != "" {
				attrs = append(attrs, "query_keys", keys)
			}
			if id := slot.identity; id != nil && !id.Anonymous {
				attrs = append(attrs, "principal", id.Principal.Name, "token_id", id.Token.ID)
			}
			s.log.Info("http request", attrs...)
		}()
		next.ServeHTTP(rec, r)
	})
}

// queryKeys renders the parameter names of a query string and never a value.
func queryKeys(r *http.Request) string {
	raw := r.URL.RawQuery
	if raw == "" {
		return ""
	}
	var keys []string
	for _, pair := range strings.Split(raw, "&") {
		if k, _, ok := strings.Cut(pair, "="); ok {
			keys = append(keys, k)
		} else {
			keys = append(keys, pair)
		}
	}
	return strings.Join(keys, ",")
}

// remoteHost is the peer address without its port, or "unix" for a request
// that arrived on the local socket.
func remoteHost(r *http.Request) string {
	addr := r.RemoteAddr
	if addr == "" || addr == "@" {
		return "unix"
	}
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

// recoveryMiddleware turns a handler panic into a 500 problem document.
//
// The stack goes to the log and nowhere near the response. A panic trace names
// internal paths, package layout and sometimes the values being handled — it is
// the single most useful thing you can hand an attacker, and the single most
// useless thing you can hand a user.
func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			// A client disconnecting mid-write is not a bug and must not be
			// dressed up as one; net/http signals it with this sentinel.
			if p == http.ErrAbortHandler { //nolint:errorlint // net/http panics with this exact value
				panic(p)
			}
			s.metrics.panics.Inc()
			s.log.Error("handler panicked",
				"request_id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"route", routePattern(r),
				"panic", p,
				"stack", string(debug.Stack()))

			// If the handler already started writing, the status is sent and
			// the body is half-written; there is no honest way to turn that
			// into a problem document. Dropping the connection at least tells
			// the client the response is incomplete.
			if rec, ok := w.(*responseRecorder); ok && rec.wroteHeader {
				return
			}
			Fail(w, r, problem.Internal())
		}()
		next.ServeHTTP(w, r)
	})
}
