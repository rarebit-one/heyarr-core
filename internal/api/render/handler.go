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

	// The DLNA response headers, without which a renderer downloads the whole
	// file and then refuses to render it.
	//
	// MEASURED, against a Samsung QN85B on 2026-08-24. Without these, with a
	// correct Content-Type and everything else in place, the television
	// fetched all 4,999,379 bytes in ONE request, went TRANSITIONING, settled
	// on STOPPED, and showed nothing. No error, no SOAP fault, nothing in any
	// log at either end.
	//
	// With them, the same television switched to RANGED requests and played.
	// That change in fetch shape — one whole-file GET becoming a series of
	// 206s — is the observable signal that a renderer has understood the
	// offer, and it is worth knowing about when the next device silently does
	// nothing.
	//
	// DLNA.ORG_PN is deliberately omitted. It names an exact profile —
	// AVC_MP4_MP_HD_AAC_MULT5 and several hundred others — and a WRONG one is
	// worse than none: the device believes the claim and fails on content that
	// does not match it. Absent, a renderer probes the stream instead, which
	// is what it does for anything it did not recognise anyway.
	//
	// DLNA.ORG_OP=01 advertises range seeking, which is true here and is what
	// lets someone scrub. DLNA.ORG_CI=0 says the bytes are not transcoded.
	//
	// Assigned into the header map directly rather than through Set, which
	// would canonicalise the names to "Contentfeatures.dlna.org". HTTP field
	// names are case-insensitive and THE SAMSUNG ACCEPTED THE CANONICAL FORM
	// — it played — so this is hardening, not a fix, and is recorded that way
	// rather than as a measured requirement. The DLNA specification writes
	// both names in mixed case and other renderers are reported to match them
	// literally; preserving the spelling costs nothing and removes a variable
	// from the next device that does not work.
	w.Header()["contentFeatures.dlna.org"] = []string{
		"DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000",
	}
	// Echoed when asked for, because a renderer that requested Background or
	// Interactive and is told Streaming may decline. Streaming is the default
	// for the audio and video this route exists to serve.
	mode := r.Header.Get("transferMode.dlna.org")
	if mode == "" {
		mode = "Streaming"
	}
	w.Header()["transferMode.dlna.org"] = []string{mode}

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
		// PlayableMIME is re-checked HERE, at the point the header is
		// written, and not only where the capability was minted. Minting and
		// serving are separated by an expiry window and by whatever the
		// catalog did in between; a token minted before this rule existed
		// must not be honoured by a binary that has it.
		if w.mime != "" && PlayableMIME(w.mime) &&
			w.Header().Get("Content-Type") == "application/octet-stream" {
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
