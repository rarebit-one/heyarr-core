package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Prefix is where the gateway serves the Subsonic protocol — Subsonic's own
// conventional base path, so a stock app pointed at the device needs no special
// configuration beyond the device's address.
const Prefix = "/rest"

// Library reads this device's DECRYPTED personal-state playlists (§73). Its
// implementation lives on the device and decrypts locally: it fetches the opaque
// ciphertext from the controller, unwraps the space key with this device's key,
// and materialises the CRDT — all here, never on the controller. The controller
// sees only ciphertext; this interface returns the plaintext the device alone can
// produce. [SpaceLibrary] is the production implementation.
type Library interface {
	// Playlists lists every playlist this device can decrypt, in a stable order,
	// without their entries.
	Playlists(ctx context.Context) ([]Playlist, error)
	// Playlist returns one playlist with its entries, or ok=false when this device
	// cannot decrypt a playlist of that id (it holds no key for it, or none
	// exists).
	Playlist(ctx context.Context, id string) (pl Playlist, ok bool, err error)
}

// Playlist is a decrypted playlist: an id (the space id), a display name, and the
// item ids in their converged order.
type Playlist struct {
	ID    string
	Name  string
	Items []string
}

// Options build a [Server].
type Options struct {
	// Personal serves playlists from on-device-decrypted state. Required: a
	// gateway with no personal-state reader is just a Subsonic proxy, and the
	// whole point of the gateway is to serve the state the controller cannot.
	Personal Library
	// Controller is where library and stream methods are proxied. Required.
	Controller Controller
	// DeviceUser and DevicePassword are the credential the STOCK APP presents to
	// the device (Subsonic u + p). This is a DISTINCT credential from the
	// controller bearer inside Controller: the app authenticates to the device,
	// the device authenticates to the controller, and the two never share a
	// secret. A phone that is compromised leaks its device password, not the
	// controller token. DevicePassword is required — an empty one would accept
	// every caller.
	DeviceUser     string
	DevicePassword string
	// ServerVersion fills the OpenSubsonic serverVersion field.
	ServerVersion string
	Logger        *slog.Logger
}

// Server is the device-side compatibility gateway (§70, §73, ADR-0051). It is an
// http.Handler a device mounts on its OWN local server — it is deliberately not
// mounted on the controller's router (that would be the invariant violation this
// design exists to prevent).
type Server struct {
	personal      Library
	proxy         *proxy
	deviceUser    string
	devicePass    string
	serverVersion string
	log           *slog.Logger
}

// New builds the gateway, refusing a mis-wired one at construction.
func New(opts Options) (*Server, error) {
	if opts.Personal == nil {
		return nil, errors.New("gateway: a personal-state Library is required")
	}
	if opts.DevicePassword == "" {
		return nil, errors.New("gateway: a device password is required, or every caller would be accepted")
	}
	pr, err := newProxy(opts.Controller)
	if err != nil {
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	sv := opts.ServerVersion
	if sv == "" {
		sv = "unknown"
	}
	user := opts.DeviceUser
	if user == "" {
		user = "heyarr"
	}
	return &Server{
		personal:      opts.Personal,
		proxy:         pr,
		deviceUser:    user,
		devicePass:    opts.DevicePassword,
		serverVersion: sv,
		log:           log.With("component", "gateway"),
	}, nil
}

// Mount registers the gateway on a router. The whole protocol is one
// parameterised route dispatched internally, exactly as the controller-side
// adapter does it, so a stray .view suffix is stripped in one place and an
// unknown method returns a Subsonic error envelope rather than an HTTP 404.
func (s *Server) Mount(r chi.Router) {
	gr := chi.NewRouter()
	gr.Get("/{method}", s.dispatch)
	gr.Head("/{method}", s.dispatch)
	r.Mount(Prefix, gr)
}

// Handler returns the gateway as a standalone http.Handler for a device that
// runs it as its own server rather than mounting it in a larger router.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	s.Mount(r)
	return r
}

// dispatch routes one Subsonic method.
//
// Two families. The personal-state methods (getPlaylists, getPlaylist) are served
// HERE, from on-device-decrypted state — they never touch the controller, which
// holds no key to serve them. Everything else — the handshake, browse and byte
// serving — is proxied to the controller's Subsonic adapter, which serves the
// server-readable catalogue perfectly well. The split is invisible to the app:
// one origin, the device.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimSuffix(chi.URLParam(r, "method"), ".view")

	// Every method authenticates the app to the DEVICE first. A proxied method
	// then re-authenticates the DEVICE to the controller inside the proxy, with
	// the controller bearer the app never holds.
	p := parse(r)
	if code, msg := s.authenticate(p); code != 0 {
		s.write(w, p.format, s.fail(code, msg))
		return
	}

	switch method {
	case "getPlaylists":
		s.handleGetPlaylists(w, r, p)
	case "getPlaylist":
		s.handleGetPlaylist(w, r, p)
	case "ping",
		"getLicense",
		"getOpenSubsonicExtensions",
		"getMusicFolders",
		"getArtists",
		"getArtist",
		"getAlbumList2",
		"getAlbum",
		"stream",
		"download":
		s.proxy.forward(w, r, method)
	default:
		s.write(w, p.format, s.fail(errGeneric, "unsupported method: "+method))
	}
}

// params is the subset of the Subsonic request the gateway reads itself. The
// proxy forwards the full query untouched except for the credential.
type params struct {
	user     string // u
	password string // p (may be "enc:<hex>")
	token    string // t — salted-token auth, which the gateway cannot honour
	format   string // f: "xml" (default) or "json"
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
		format:   f,
	}
}

// authenticate verifies the stock app's credential against the DEVICE password.
//
// The app authenticates to the device in Subsonic's own terms — the u+p pair on
// the query string — exactly as it would to any Subsonic server. This is a
// device-local credential, entirely separate from the controller bearer the
// proxy holds. The salted-token form (t+s) is refused for the same reason the
// controller-side adapter refuses it: honouring it needs the plaintext password,
// which is compared here in constant time and not otherwise recoverable.
func (s *Server) authenticate(p params) (code int, message string) {
	if p.token != "" {
		return errBadAuth, "salted-token authentication is not supported; configure this client to send the password directly"
	}
	if p.password == "" {
		return errMissingParam, "required parameter is missing: p"
	}
	pw := p.password
	if enc, ok := strings.CutPrefix(pw, "enc:"); ok {
		decoded, err := hex.DecodeString(enc)
		if err != nil {
			return errBadAuth, "wrong username or password"
		}
		pw = string(decoded)
	}
	// Both compares run before the branch — a constant-time compare on each half,
	// and the branch is over the already-computed bools — so the reply does not
	// reveal which half was wrong.
	userOK := subtle.ConstantTimeCompare([]byte(p.user), []byte(s.deviceUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pw), []byte(s.devicePass)) == 1
	if !userOK || !passOK {
		return errBadAuth, "wrong username or password"
	}
	return 0, ""
}

func (s *Server) handleGetPlaylists(w http.ResponseWriter, r *http.Request, p params) {
	pls, err := s.personal.Playlists(r.Context())
	if err != nil {
		s.internalError(w, p, "getPlaylists", err)
		return
	}
	out := make([]playlist, 0, len(pls))
	for _, pl := range pls {
		out = append(out, playlist{ID: pl.ID, Name: pl.Name, SongCount: len(pl.Items)})
	}
	resp := s.ok()
	resp.Playlists = &playlists{Playlist: out}
	s.write(w, p.format, resp)
}

func (s *Server) handleGetPlaylist(w http.ResponseWriter, r *http.Request, p params) {
	id := r.URL.Query().Get("id")
	if id == "" {
		s.write(w, p.format, s.fail(errMissingParam, "required parameter is missing: id"))
		return
	}
	pl, ok, err := s.personal.Playlist(r.Context(), id)
	if err != nil {
		s.internalError(w, p, "getPlaylist", err)
		return
	}
	if !ok {
		s.write(w, p.format, s.fail(errNotFound, "playlist not found"))
		return
	}
	entries := make([]entry, 0, len(pl.Items))
	for _, item := range pl.Items {
		entries = append(entries, entry{ID: item, Title: item, IsDir: false})
	}
	resp := s.ok()
	resp.Playlist = &playlist{ID: pl.ID, Name: pl.Name, SongCount: len(entries), Entry: entries}
	s.write(w, p.format, resp)
}

// internalError logs a device-side failure and returns the generic Subsonic
// error, never the underlying message — a client cannot act on it, and a decrypt
// or transport error should not leak into a protocol reply.
func (s *Server) internalError(w http.ResponseWriter, p params, op string, err error) {
	s.log.Error("gateway request failed", "op", op, "error", err)
	s.write(w, p.format, s.fail(errGeneric, "the server failed to answer that request"))
}
