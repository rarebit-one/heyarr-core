package render

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// Options builds a Handler.
type Options struct {
	// Blobs is the ordinary blob handler. Reusing it is the point: ADR-0013
	// makes byte serving a contract with several consumers, and a
	// renderer-specific copy would be a second range implementation to keep in
	// step with the first.
	Blobs *blobs.Handler
	// Secret signs and verifies capabilities.
	Secret []byte
	Logger *slog.Logger
	// Now is injected so expiry is testable without sleeping.
	Now func() time.Time
}

// Handler serves blobs to devices that can only fetch a URL.
type Handler struct {
	blobs  *blobs.Handler
	secret []byte
	log    *slog.Logger
	now    func() time.Time
}

// New builds the handler, refusing a mis-wired one at construction.
func New(opts Options) (*Handler, error) {
	if opts.Blobs == nil {
		return nil, errors.New("render: a blob handler is required")
	}
	if len(opts.Secret) == 0 {
		return nil, errors.New("render: a signing secret is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Handler{blobs: opts.Blobs, secret: opts.Secret, log: log.With("component", "render"), now: now}, nil
}

// Mount registers the renderer route on an UNAUTHENTICATED router.
//
// That is the whole design and it should be uncomfortable enough to check: the
// capability in the path is the only authority, so this must not be mounted
// inside the bearer-authenticated group, where it would be dead code, nor
// anywhere a caller could reach it without one.
func (h *Handler) Mount(r chi.Router) {
	r.Get(httpapi.RenderPrefix+"/{capability}", h.serve)
	r.Head(httpapi.RenderPrefix+"/{capability}", h.serve)
	// A trailing name is accepted and ignored. Some renderers infer a type
	// from the last path segment before they will even issue the GET, and a
	// URL ending in an opaque token looks like nothing they recognise. It is
	// outside the signature because it decides nothing: the Content-Type comes
	// from the capability, and the bytes come from its blob.
	r.Get(httpapi.RenderPrefix+"/{capability}/{name}", h.serve)
	r.Head(httpapi.RenderPrefix+"/{capability}/{name}", h.serve)
}

// Path is where a capability is fetched from, relative to an origin. It is
// here rather than composed by callers so that the route and the URL that
// reaches it cannot drift apart.
func Path(capability string) string {
	return httpapi.RenderPrefix + "/" + capability
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) {
	granted, err := Verify(h.secret, chi.URLParam(r, "capability"), h.now())
	if err != nil {
		h.refuse(w, r, err)
		return
	}

	// The blob handler reads its subject from the route, so the verified hash
	// is put where it looks. Doing it this way rather than reaching into the
	// CAS keeps one implementation of ranges, ETags and conditional requests.
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("hash", granted.BlobHash)
	inner := r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))

	h.blobs.Content(&retyped{ResponseWriter: w, mime: granted.MIME}, inner)
}

// refuse answers a bad capability.
//
// Every failure is a 404 and none of them says which failure it was. The
// distinction the errors draw is for the operator reading the log, not for the
// caller: telling an anonymous client "that signature is wrong" rather than
// "no such thing" confirms that a blob exists and turns the endpoint into an
// oracle for guessing at tokens.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, err error) {
	// A bad signature is the only one worth a raised voice: expiry is the
	// system working as designed, and a malformed token is usually a crawler.
	level := slog.LevelInfo
	if errors.Is(err, ErrSignature) {
		level = slog.LevelWarn
	}
	h.log.Log(r.Context(), level, "refused a render capability",
		"request_id", httpapi.RequestIDFrom(r.Context()),
		"reason", err.Error(),
		"remote", r.RemoteAddr)
	// The instance is set explicitly, and to a constant.
	//
	// problem.Write otherwise fills it from the request path — which on this
	// route CONTAINS the capability, so the 404 body would hand the secret
	// back in a document that intermediaries cache and log. The access log is
	// redacted for the same reason (httpapi.logPath); redacting one and
	// echoing the other in a response body would be no protection at all.
	//
	// A constant also makes every refusal byte-identical, which is what stops
	// the endpoint being an oracle: a caller cannot tell a bad signature from
	// an expiry from a blob this peer does not hold.
	httpapi.Fail(w, r, problem.NotFound("no content is available at this address").
		WithInstance(httpapi.RenderPrefix))
}

// retyped overrides the blob handler's Content-Type, and only that.
//
// The blob endpoint answers application/octet-stream deliberately: bytes have
// no type, and a peer that sniffed one could type the same blob differently
// from its neighbour (ADR-0006). A renderer, though, refuses octet-stream — so
// the Asset's declared type, signed into the capability when an Asset was in
// hand, is substituted here at the edge. The blob contract is unchanged; this
// is a view of it.
type retyped struct {
	http.ResponseWriter
	mime    string
	written bool
}

func (w *retyped) WriteHeader(status int) {
	if !w.written {
		w.written = true
		// Only the octet-stream default is replaced. A multi-range response is
		// multipart/byteranges, and overwriting that would produce a body no
		// client could parse — a renderer seeking around a file is exactly the
		// caller that might ask for one.
		if w.mime != "" && w.Header().Get("Content-Type") == "application/octet-stream" {
			w.Header().Set("Content-Type", w.mime)
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *retyped) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}
