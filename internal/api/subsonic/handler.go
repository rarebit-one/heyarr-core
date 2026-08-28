package subsonic

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Prefix is where the OpenSubsonic adapter is mounted. It is Subsonic's own
// conventional base path, and it lives OUTSIDE Heyarr's authenticated /api/v1
// group for the same reason the renderer and pairing routes do (see
// httpapi.RenderPrefix): a Subsonic client cannot present a Heyarr bearer
// credential — it authenticates in the protocol's own terms, on the query
// string — so the group's bearer guard would 401 every real client it exists to
// serve. The adapter does its own authentication instead, and maps it onto the
// same tokens and scopes the rest of the server uses.
const Prefix = "/rest"

// Authenticator verifies a Heyarr bearer credential. It is the subset of
// *auth.Verifier this adapter needs, as an interface so a test can inject a
// fake without paying argon2id.
type Authenticator interface {
	Verify(ctx context.Context, raw string) (auth.Identity, error)
}

// Options builds a Handler.
type Options struct {
	// DB is the controller database, read through its reader pool. The adapter
	// is a read-only PROJECTION over the catalogue (§70): it never writes, and
	// it never reaches the encrypted personal-state plane, which the server
	// cannot decrypt anyway (Invariant 6, §72). Playlists, play-counts and
	// scrobbles are therefore out of scope here by construction and belong to a
	// device-side Personal MCP (§73), not this controller-side adapter.
	DB *sqlite.DB
	// Auth verifies the bearer token a Subsonic client sends as its password.
	Auth Authenticator
	// Blobs is the ordinary blob handler. stream/download resolve a track to a
	// blob hash and hand the byte-serving to it verbatim (ADR-0013): Range, 206
	// and M10 progressive partial serving are inherited, not reimplemented, and
	// the adapter stays as piece-agnostic as blobs itself.
	Blobs *blobs.Handler
	// ServerVersion fills the OpenSubsonic `serverVersion` field.
	ServerVersion string
	Logger        *slog.Logger
}

// Handler is the OpenSubsonic compatibility adapter (§70).
type Handler struct {
	reader        *sql.DB
	auth          Authenticator
	blobs         *blobs.Handler
	serverVersion string
	log           *slog.Logger
}

// New builds the handler, refusing a mis-wired one at construction.
func New(opts Options) (*Handler, error) {
	if opts.DB == nil {
		return nil, errors.New("subsonic: a database is required")
	}
	if opts.Auth == nil {
		return nil, errors.New("subsonic: an authenticator is required")
	}
	if opts.Blobs == nil {
		return nil, errors.New("subsonic: a blob handler is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	sv := opts.ServerVersion
	if sv == "" {
		sv = "unknown"
	}
	return &Handler{
		reader:        opts.DB.Reader(),
		auth:          opts.Auth,
		blobs:         opts.Blobs,
		serverVersion: sv,
		log:           log.With("component", "subsonic"),
	}, nil
}

// Mount registers the adapter on an UNAUTHENTICATED router (see Prefix).
//
// The whole protocol is one parameterised route, `/rest/{method}`, dispatched
// internally rather than registered method by method. Two reasons: Subsonic is
// a foreign protocol whose methods are the OpenSubsonic specification's to
// define, not Heyarr's OpenAPI's (so the route parity guard documents one
// compatibility endpoint, not forty); and a client may append the legacy
// `.view` suffix to any method, which a single dispatcher strips in one place.
// An unknown method returns a Subsonic error envelope, not an HTTP 404, so the
// client parses the refusal instead of choking on a stray page.
func (h *Handler) Mount(r chi.Router) {
	sr := chi.NewRouter()
	sr.Get("/{method}", h.dispatch)
	sr.Head("/{method}", h.dispatch)
	r.Mount(Prefix, sr)
}

// dispatch routes one Subsonic method to its handler. stream and download share
// one handler: both serve the track's bytes, and this slice does not transcode,
// so download's "always the original" and stream's "maybe transcoded" collapse
// to the same DIRECT bytes.
func (h *Handler) dispatch(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimSuffix(chi.URLParam(r, "method"), ".view")
	switch method {
	case "ping":
		h.handlePing(w, r)
	case "getLicense":
		h.handleGetLicense(w, r)
	case "getOpenSubsonicExtensions":
		h.handleGetOpenSubsonicExtensions(w, r)
	case "getMusicFolders":
		h.handleGetMusicFolders(w, r)
	case "getArtists":
		h.handleGetArtists(w, r)
	case "getArtist":
		h.handleGetArtist(w, r)
	case "getAlbumList2":
		h.handleGetAlbumList2(w, r)
	case "getAlbum":
		h.handleGetAlbum(w, r)
	case "stream", "download":
		h.handleStream(w, r)
	default:
		p := parse(r)
		h.write(w, p.format, h.fail(errGeneric, "unsupported method: "+method))
	}
}

// params is the common Subsonic request parameter set.
type params struct {
	user     string // u
	password string // p (may be "enc:<hex>")
	token    string // t (salted-token auth — unsupported here)
	salt     string // s
	client   string // c
	version  string // v
	format   string // f: "xml" (default), "json"
}

func parse(r *http.Request) params {
	q := r.URL.Query()
	f := strings.ToLower(q.Get("f"))
	if f != "json" {
		f = "xml"
	}
	return params{
		user:     q.Get("u"),
		password: q.Get("p"),
		token:    q.Get("t"),
		salt:     q.Get("s"),
		client:   q.Get("c"),
		version:  q.Get("v"),
		format:   f,
	}
}

// authenticate turns a Subsonic credential into a Heyarr identity with at least
// `read`.
//
// A Subsonic client offers one of two credentials. It may send the password
// directly (p=, optionally hex-encoded as p=enc:<hex>); that password IS a
// Heyarr bearer token, which is verified against the same store the whole API
// uses, so a token's scope and revocation apply here identically. Or it may
// send the salted-token pair (t = md5(password+salt), s = salt), which this
// adapter cannot honour: verifying it requires the server to recompute the MD5
// from the plaintext password, and Heyarr keeps tokens argon2id-hashed at rest
// and never holds the plaintext (auth/token.go). That is a deliberate,
// documented refusal, not a gap — configure the client to send the password.
func (h *Handler) authenticate(ctx context.Context, p params) (auth.Identity, int, string) {
	if p.token != "" {
		return auth.Identity{}, errBadAuth,
			"salted-token authentication is not supported; configure this client to send the password directly"
	}
	pw := p.password
	if pw == "" {
		return auth.Identity{}, errMissingParam, "required parameter is missing: p"
	}
	if enc, ok := strings.CutPrefix(pw, "enc:"); ok {
		decoded, err := decodeHex(enc)
		if err != nil {
			return auth.Identity{}, errBadAuth, "wrong username or password"
		}
		pw = decoded
	}
	id, err := h.auth.Verify(ctx, pw)
	if err != nil {
		return auth.Identity{}, errBadAuth, "wrong username or password"
	}
	if !id.Allows(auth.ScopeRead) {
		return auth.Identity{}, errNotAuthorized, "this credential is not allowed to read the library"
	}
	return id, 0, ""
}

// authed authenticates, then either runs the envelope handler or writes the
// Subsonic error the failure maps to.
func (h *Handler) authed(w http.ResponseWriter, r *http.Request, fn func(w http.ResponseWriter, r *http.Request, p params)) {
	p := parse(r)
	if _, code, msg := h.authenticate(r.Context(), p); code != 0 {
		h.write(w, p.format, h.fail(code, msg))
		return
	}
	fn(w, r, p)
}

func (h *Handler) handlePing(w http.ResponseWriter, r *http.Request) {
	h.authed(w, r, func(w http.ResponseWriter, r *http.Request, p params) {
		h.write(w, p.format, h.ok())
	})
}

func (h *Handler) handleGetLicense(w http.ResponseWriter, r *http.Request) {
	h.authed(w, r, func(w http.ResponseWriter, r *http.Request, p params) {
		resp := h.ok()
		// Heyarr is not a licensed product; the field exists because clients
		// gate features on it, and "valid" is the answer that unblocks them.
		resp.License = &license{Valid: true}
		h.write(w, p.format, resp)
	})
}

func (h *Handler) handleGetOpenSubsonicExtensions(w http.ResponseWriter, r *http.Request) {
	// The extension list is the one handshake endpoint Subsonic left
	// unauthenticated, so a client can discover OpenSubsonic support before it
	// has a working credential. The adapter implements no optional extension
	// yet, so the list is empty — but present, which is itself the signal that
	// the server speaks OpenSubsonic.
	p := parse(r)
	resp := h.ok()
	empty := []osExtension{}
	resp.OpenSubsonicExtensions = &empty
	h.write(w, p.format, resp)
}

func (h *Handler) handleGetMusicFolders(w http.ResponseWriter, r *http.Request) {
	h.authed(w, r, func(w http.ResponseWriter, r *http.Request, p params) {
		folders, err := h.musicFolders(r.Context())
		if err != nil {
			h.internalError(w, r, p, "getMusicFolders", err)
			return
		}
		resp := h.ok()
		resp.MusicFolders = &musicFolders{MusicFolder: folders}
		h.write(w, p.format, resp)
	})
}

// internal logs a query failure and returns the generic Subsonic error, never
// the underlying message — a client cannot act on a SQL error and it should not
// see the schema.
func (h *Handler) internalError(w http.ResponseWriter, r *http.Request, p params, op string, err error) {
	h.log.Error("subsonic query failed", "op", op, "error", err)
	h.write(w, p.format, h.fail(errGeneric, "the server failed to answer that request"))
}
