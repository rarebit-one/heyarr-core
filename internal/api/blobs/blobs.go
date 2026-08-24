// Package blobs serves blob bytes over HTTP with byte-range support (spec §28,
// ADR-0013).
//
// # This is a contract, not "the video endpoint"
//
// GET|HEAD /api/v1/blobs/{hash}/content has four consumers, and only one of
// them is a media player:
//
//   - direct playback, which seeks and expects 206 responses;
//   - Milestone 2's remote ffprobe (§29), which reads the first, metadata and
//     footer ranges of a container rather than materialising 20 GB to answer
//     "what codec is this";
//   - Milestone 4's replication, which resumes an interrupted copy from a byte
//     offset;
//   - Milestone 6's HTTP web-seed (§27), which serves BitTorrent piece
//     boundaries to a swarm that also has peers.
//
// Every one of those is a plain, honest byte range over an immutable object,
// which is why they share an endpoint. The failure mode this comment exists to
// prevent is tuning it for the player: transcoding here, guessing a media
// Content-Type, capping the response size, adding a player-shaped session
// token, or short-circuiting a HEAD. Each of those breaks at least one of the
// other three consumers, and it breaks them at the milestone that adds them —
// long after whoever made the change has moved on.
//
// The endpoint therefore serves bytes and says nothing about what they mean.
// Media type, filename and container metadata belong to the catalog resources
// (/api/v1/blobs/{hash} and the asset resources), because those are properties
// of a *use* of the bytes, not of the bytes: one blob can be two assets with
// two filenames, and the CAS deliberately does not know either (ADR-0006).
package blobs

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// Options configure a Handler.
type Options struct {
	// Store supplies the bytes. Required.
	Store cas.Store
	// Logger records the failures a client is not told about. Optional.
	Logger *slog.Logger
}

// Handler serves blob content.
type Handler struct {
	store cas.Store
	log   *slog.Logger
}

// New builds the handler. It returns an error rather than panicking so that a
// mis-wired server fails at construction, before anything is listening.
func New(opts Options) (*Handler, error) {
	if opts.Store == nil {
		return nil, errors.New("blobs: a CAS store is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Handler{store: opts.Store, log: log.With("component", "blobs")}, nil
}

// Mount registers the routes on the authenticated /api/v1 router. It satisfies
// httpapi.MountFunc, so everything here inherits the middleware chain and the
// read-scope floor.
//
// Reading bytes needs `read` and nothing more, which the router already
// requires. There is no write route: blobs arrive through ingest, and an object
// whose name is its digest cannot be updated in place by definition (ADR-0005).
func (h *Handler) Mount(r chi.Router) {
	r.Method(http.MethodGet, "/blobs/{hash}/content", http.HandlerFunc(h.Content))
	r.Method(http.MethodHead, "/blobs/{hash}/content", http.HandlerFunc(h.Content))
}

// immutableCacheControl is a year, the maximum RFC 9111 asks anyone to honour,
// plus `immutable` so a revalidating client does not even ask.
//
// This is correct rather than reckless *because the URL is the hash*: there is
// no revision of these bytes that could ever be served from this path, so a
// cache that keeps them forever can never be wrong. The usual danger of
// immutable caching — shipping a fix that a cache refuses to pick up — cannot
// arise, because a fix would have a different digest and therefore a different
// URL.
const immutableCacheControl = "public, max-age=31536000, immutable"

// Content serves a blob, or a range of one.
//
// It is exported because ADR-0013's title is the whole of the reason: blob
// serving is a CONTRACT, not an endpoint. The peer fabric mounts this same
// function on its own listener under its own credential (M4-09), and a
// replication-specific copy of it would be a second range implementation to
// keep in step with this one — which is exactly what "the same endpoint that
// Milestone 4 replication reads from" was written to prevent. One function,
// two mount points, two trust roots.
//
// Range parsing is http.ServeContent's job, deliberately. Multi-range multipart
// responses, If-Range, the 206 and 416 boundary conditions and the difference
// between `bytes=0-` and `bytes=-0` are a decade of accumulated corrections in
// the standard library, and every hand-rolled range parser rediscovers a subset
// of them. What this function owns is identity, the headers, and the fact that
// the reader is a seekable stream and never a buffer.
func (h *Handler) Content(w http.ResponseWriter, r *http.Request) {
	h.ContentAs(w, r, OctetStream)
}

// OctetStream is what the blob endpoint declares, and the reason is in the
// Content-Type note below: bytes have no type, and this endpoint has no Asset
// to ask.
const OctetStream = "application/octet-stream"

// ContentAs serves a blob under a caller-declared media type.
//
// # This is not a hole in ADR-0013
//
// That ADR is about the blob ENDPOINT: one route, several consumers, not tuned
// for a player. The endpoint is unchanged — Content still declares
// OctetStream, unconditionally, for every caller it has. This is an internal
// entry point for the one consumer that holds an Asset and therefore knows
// something the CAS cannot: what the bytes are.
//
// It exists so that the renderer route (ADR-0040) does not have to wrap the
// ResponseWriter to correct a header afterwards. That wrapper worked and read
// badly — a header rewritten in one method, for a body written in another —
// and it put a second io.Writer in the response path for no reason but to
// change a string. Declaring the type up front is what the caller meant.
//
// The mime is a constant at every call site by construction: Content passes a
// literal, and the renderer route passes a value from its own table of
// literals (render.CanonicalMIME). Nothing a client sends reaches this
// parameter.
func (h *Handler) ContentAs(w http.ResponseWriter, r *http.Request, mime string) {
	if mime == "" {
		mime = OctetStream
	}
	raw := chi.URLParam(r, "hash")
	hash, err := hashing.Parse(raw)
	if err != nil {
		// A malformed hash and an absent blob are different mistakes and get
		// different statuses. 400 says "that is not a hash"; 404 says "that is
		// a hash and this peer does not have it" — which is exactly the
		// question replication and a web-seed client are asking. Collapsing
		// them into one status makes a client retry a request that can never
		// succeed. It also stops here rather than at the CAS: nothing
		// unvalidated should reach a store that turns identifiers into paths.
		httpapi.Fail(w, r, problem.BadRequest(
			"the blob identifier must be blake3:<64 lowercase hex characters>"))
		return
	}

	rsc, _, err := h.store.Open(r.Context(), hash)
	switch {
	case errors.Is(err, cas.ErrNotFound):
		httpapi.Fail(w, r, problem.NotFound("this peer holds no blob "+hash.String()))
		return
	case err != nil:
		h.log.Error("opening a blob failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"hash", hash.String(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	defer func() {
		if err := rsc.Close(); err != nil {
			h.log.Warn("closing a blob failed", "hash", hash.String(), "error", err)
		}
	}()

	// A strong validator derived from the content itself. ServeContent reads it
	// back out of the header to answer If-Range and If-None-Match, so it has to
	// be set before the call, not after.
	w.Header().Set("ETag", `"blake3-`+hash.Hex()+`"`)
	w.Header().Set("Cache-Control", immutableCacheControl)
	// ServeContent sets this too. It is set here as well because §28 makes it
	// part of the contract every peer advertises, and a contract that holds
	// only as a side effect of a library's internals is one refactor from being
	// silently withdrawn.
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Declared, never sniffed. Sniffing would make the type depend on the
	// first 512 bytes, so the same blob could be typed differently by two
	// peers, and a peer that guessed "video/mp4" would be inviting a browser
	// to render content the catalog has not classified. Setting it explicitly
	// also stops ServeContent from sniffing.
	//
	// For the endpoint this is always OctetStream, which is the honest answer
	// there: the CAS knows the digest and the length and nothing else. A
	// caller that passes something else is one holding an Asset, which knows
	// what the CAS cannot — see ContentAs.
	w.Header().Set("Content-Type", mime)
	if wantsDownload(r) {
		// The blob has no filename — a filename is a property of an asset, not
		// of the bytes. The digest is the only name that is always correct, and
		// it is ASCII by construction, so there is nothing here to escape.
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=%q", hash.Hex()+".bin"))
	}

	// The modtime is deliberately the zero time.
	//
	// ServeContent omits Last-Modified for a zero time and skips the
	// date-based half of If-Range and If-Modified-Since. That is what we want:
	// a real mtime is a property of *this peer's copy* — when it was
	// materialised, restored, or reflinked — and passing it would make two
	// peers holding byte-identical content advertise different validators.
	// Replication and a web-seed swarm both read the same blob from several
	// peers, and a cache keyed on a validator that varies per peer either
	// thrashes or, worse, decides a mid-transfer switch of source invalidates
	// what it already has. The strong ETag is a validator of the bytes rather
	// than of the file, so conditional requests still work correctly — they
	// just work identically everywhere. Nothing here is mutable, so there is
	// nothing a modification date could tell a client that the digest does not.
	var noModTime time.Time

	// Anything after this point is ServeContent's: status, Content-Range,
	// Content-Length and the body. rsc stays a seekable stream all the way
	// down, so the response is copied through a fixed-size buffer (or handed to
	// sendfile) and memory stays flat in the blob's size. Reading it into a
	// []byte first — with io.ReadAll, or by "just" caching small blobs — would
	// turn a 20 GB remux, which ADR-0013 calls a normal case, into 20 GB of
	// heap per concurrent request.
	http.ServeContent(w, r, "", noModTime, rsc)
}

// wantsDownload reports whether the caller asked for an attachment.
//
// It is opt-in per request rather than a separate route because it changes one
// response header and nothing else — the bytes, the ranges and the validators
// are identical, so a second route would be a second thing to keep in step for
// no gain.
func wantsDownload(r *http.Request) bool {
	switch r.URL.Query().Get("download") {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
