package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// MountFunc registers additional routes on the authenticated /api/v1 router.
//
// This is the extension point for the rest of the API: resource handlers and
// blob serving mount here rather than reaching into the server, so every route
// they add inherits the whole middleware chain and the read-scope floor without
// having to remember to. A route needing more than `read` says so at the route:
//
//	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/works", handler)
//
// The router is chi's, and the path parameters are read with chi.URLParam.
type MountFunc func(r chi.Router)

// EventHead reports the highest sequence number in the event log.
//
// It is an interface rather than *events.Log so that this package — the HTTP
// foundation everything else mounts onto — does not take a dependency on the
// event log to answer one field of one endpoint. events.Log satisfies it.
type EventHead interface {
	Latest(ctx context.Context) (int64, error)
}

// Options configure a Server.
type Options struct {
	// Config supplies the listen addresses and the auth switch. The server
	// re-checks the bind policy itself rather than trusting that whoever built
	// this already validated it.
	Config config.Config
	Logger *slog.Logger
	// DB is the controller database. Required: authentication and readiness
	// both need it.
	DB *sqlite.DB
	// Verifier authenticates bearer tokens. Required unless authentication is
	// disabled.
	Verifier *auth.Verifier
	// Events reports the log's head sequence for GET /api/v1/system. Required:
	// the alternative is a server that reports head 0 because it was wired
	// without a log, which is indistinguishable from an empty log and would
	// send a client that trusted it back to sequence zero. There is no
	// configuration in which that field should be a guess.
	Events EventHead
	// Media is the external toolchain this node resolved, reported by
	// GET /api/v1/system. Nil means "this node resolved none", which is a
	// legitimate state rather than a wiring mistake — unlike Events, an empty
	// answer here is indistinguishable from the true one, so it is not
	// required.
	Media []ToolInfo
	// Build identifies the running binary for GET /api/v1/system.
	Build buildinfo.Info
	// SchemaVersion is the migration version the database is at.
	SchemaVersion int64
	// KnownSchemaVersion is the highest migration COMPILED INTO this binary —
	// sqlite.KnownSchemaVersion(). It is what SchemaVersion is compared against
	// to report schema drift (#150), and it is required rather than defaulted
	// for the same reason Events is: a zero here would report "unknown" on
	// every request, and a drift check that has quietly stopped comparing looks
	// exactly like a fleet with no drift. The failure this endpoint exists to
	// catch is precisely an absent mechanism reading as a clean bill of health,
	// so it must not be possible to wire it up without it.
	KnownSchemaVersion int64
	// CASRoot is checked for presence and writability by GET /readyz. Empty
	// disables the check.
	CASRoot string
	// Mount registers additional API routes. See MountFunc.
	Mount []MountFunc
	// PeerMembership is the peer fabric's trust root (ADR-0012, M4-04). When
	// it is set, every request that presents a peer identity is checked
	// against it — on every request, which is what makes removing a membership
	// record an actual revocation rather than a revocation at the next
	// restart.
	//
	// Nil disables the check, which is the correct state for a deployment with
	// one peer: there is nothing to be a member of. It is not a default that
	// weakens anything, because with nothing wired to present a peer identity
	// the guard would pass every request through anyway.
	PeerMembership PeerMembership
	// PeerLiveness records that a peer was heard from, once the guard above
	// has admitted its request (§31, M4-10). Nil disables the recording and
	// changes nothing else: liveness still flows from the idle probe.
	PeerLiveness PeerLiveness
	// PresentedPeerKey extracts the peer identity a connection proved. Nil
	// means TLSPresentedPeerKey, which is the only production extractor. It is
	// injectable so the revocation behaviour can be driven by a test without
	// standing up mTLS, which M4-05 owns.
	PresentedPeerKey PresentedPeerKey
	// Now is injected so expiry and durations are testable.
	Now func() time.Time
}

// Server is the HTTP API.
type Server struct {
	cfg      config.Config
	log      *slog.Logger
	db       *sqlite.DB
	verifier *auth.Verifier
	events   EventHead
	media    []ToolInfo
	build    buildinfo.Info
	schema   int64
	known    int64
	casRoot  string
	now      func() time.Time

	peers        PeerMembership
	peerLiveness PeerLiveness
	presentedKey PresentedPeerKey

	registry *prometheus.Registry
	metrics  *metrics
	handler  http.Handler

	http       *http.Server
	socketPath string
	tcpAddr    string

	errc     chan error
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// readHeaderTimeout bounds how long a client may take to send its headers.
// Without it a handful of idle connections is a denial of service, which is why
// gosec flags its absence.
const readHeaderTimeout = 20 * time.Second

// New builds the server and its router. It binds nothing — Start does that, so
// that a construction failure is reported before any socket exists (ADR-0002's
// habit, applied here).
func New(opts Options) (*Server, error) {
	if opts.DB == nil {
		return nil, errors.New("httpapi: a database is required")
	}
	if opts.Config.HTTP.Auth.Enabled && opts.Verifier == nil {
		return nil, errors.New("httpapi: authentication is enabled but no verifier was supplied")
	}
	if opts.Events == nil {
		return nil, errors.New("httpapi: an event log is required")
	}
	if opts.KnownSchemaVersion <= 0 {
		return nil, errors.New("httpapi: the schema version this binary knows is required; " +
			"without it the drift check reports \"unknown\" forever, which is indistinguishable " +
			"from a fleet that has never drifted")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	registry := prometheus.NewRegistry()
	m, err := newMetrics(registry)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:      opts.Config,
		log:      log.With("component", "http"),
		db:       opts.DB,
		verifier: opts.Verifier,
		events:   opts.Events,
		// Normalised so the JSON shape is stable: a nil slice marshals as
		// null, and a client parsing `media` should not have to handle both
		// null and [] for the same "nothing here".
		media:        append([]ToolInfo{}, opts.Media...),
		build:        opts.Build,
		schema:       opts.SchemaVersion,
		known:        opts.KnownSchemaVersion,
		casRoot:      opts.CASRoot,
		now:          now,
		peers:        opts.PeerMembership,
		peerLiveness: opts.PeerLiveness,
		registry:     registry,
		metrics:      m,
		errc:         make(chan error, 1),
	}
	s.presentedKey = opts.PresentedPeerKey
	if s.presentedKey == nil {
		s.presentedKey = TLSPresentedPeerKey
	}
	s.handler = s.routes(opts.Mount)
	s.http = &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		// No write timeout: ADR-0013 range-serves 20 GB blobs, and a write
		// deadline that made sense for a JSON response would truncate one.
		// Bounding a slow reader is the client's connection to lose.
	}
	return s, nil
}

// Handler exposes the fully wired router, so tests can drive the real
// middleware chain through httptest without binding a port.
func (s *Server) Handler() http.Handler { return s.handler }

// Registry exposes the private Prometheus registry. It is deliberately not the
// global default registerer: a package that registers a collector in its init
// should not be able to publish it on Heyarr's /metrics by accident, and two
// servers in one test binary must not collide.
func (s *Server) Registry() *prometheus.Registry { return s.registry }

// Start binds every configured listener and begins serving in the background.
// It returns once all listeners are bound, so a caller may honestly log that
// the server is up.
func (s *Server) Start() error {
	if err := s.checkBindPolicy(); err != nil {
		return err
	}

	var listeners []net.Listener
	fail := func(err error) error {
		for _, l := range listeners {
			_ = l.Close()
		}
		return err
	}

	if addr := s.cfg.HTTP.Addr; addr != "" {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return fail(fmt.Errorf("httpapi: listening on %s: %w", addr, err))
		}
		s.tcpAddr = l.Addr().String()
		listeners = append(listeners, l)
	}
	if path := s.cfg.HTTP.UnixSocket; path != "" {
		l, err := s.listenUnix(path)
		switch {
		case errors.Is(err, errSocketPathTooLong) && len(listeners) > 0:
			// The TCP listener is up, so the API is reachable. Refusing to
			// start over a path length would be a worse outcome than losing the
			// local transport, and the operator can move the socket with
			// http.unix_socket. It is a warning rather than silence because
			// "my socket is not there" needs an answer in the log.
			s.log.Warn("not listening on the unix socket: the path is too long for this platform",
				"path", path, "length", len(path), "limit", maxUnixSocketPath(),
				"remedy", "set http.unix_socket to a shorter path")
		case err != nil:
			return fail(err)
		default:
			s.socketPath = path
			listeners = append(listeners, l)
		}
	}
	if len(listeners) == 0 {
		return errors.New("httpapi: neither http.addr nor http.unix_socket is set — the API would be unreachable")
	}

	for _, l := range listeners {
		s.wg.Add(1)
		go func(l net.Listener) {
			defer s.wg.Done()
			if err := s.http.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				select {
				case s.errc <- fmt.Errorf("httpapi: serving on %s: %w", l.Addr(), err):
				default:
				}
			}
		}(l)
	}

	s.log.Info("http listening",
		"addr", s.tcpAddr, "unix_socket", s.socketPath,
		"auth_enabled", s.cfg.HTTP.Auth.Enabled)
	return nil
}

// Err delivers the first fatal serving error. Nothing is sent on a clean
// shutdown.
func (s *Server) Err() <-chan error { return s.errc }

// Addr is the address actually bound, which is not the configured one when the
// port was zero. Empty when no TCP listener is configured.
func (s *Server) Addr() string { return s.tcpAddr }

// SocketPath is the unix socket actually bound, or empty.
func (s *Server) SocketPath() string { return s.socketPath }

// Shutdown stops accepting, waits for in-flight requests to finish within ctx,
// and removes the unix socket. It is safe to call more than once.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		err = s.http.Shutdown(ctx)
		s.wg.Wait()
		// The socket file outlives the process that made it. Leaving it behind
		// is how the next start finds a path that exists, refuses to bind, and
		// blames the operator for something the previous run did.
		if s.socketPath != "" {
			if rmErr := os.Remove(s.socketPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("httpapi: removing %s: %w", s.socketPath, rmErr))
			}
		}
	})
	return err
}

// checkBindPolicy enforces ADR-0011 at the point the listener is created.
//
// config.Validate already refuses this, and this is not redundant: the rule is
// "this process does not open an unauthenticated socket on a routable
// address", and a rule enforced only in a validator holds exactly until
// something constructs a Server another way — a test helper, a future embedded
// mode, a caller that built a config literal. The check belongs where the
// socket is opened.
func (s *Server) checkBindPolicy() error {
	if s.cfg.HTTP.Auth.Enabled || s.cfg.HTTP.Addr == "" {
		return nil
	}
	nonLoopback, err := bindsNonLoopback(s.cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("httpapi: http.addr %q is not a valid listen address: %w", s.cfg.HTTP.Addr, err)
	}
	if nonLoopback {
		return fmt.Errorf("httpapi: refusing to start — http.addr %q is not loopback and "+
			"http.auth.enabled is false, which would serve the entire library unauthenticated "+
			"(ADR-0011); either set http.auth.enabled true or bind 127.0.0.1", s.cfg.HTTP.Addr)
	}
	return nil
}

// bindsNonLoopback reports whether addr would listen on anything other than a
// loopback address. Every wildcard form — an empty host, 0.0.0.0, :: — counts
// as non-loopback, and an unresolvable host counts as non-loopback too, so the
// safe branch is the one taken when in doubt.
func bindsNonLoopback(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	switch host {
	case "":
		return true, nil
	case "localhost":
		return false, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true, nil
	}
	return !ip.IsLoopback(), nil
}

// errSocketPathTooLong reports a socket path longer than the platform's
// sockaddr_un can hold.
var errSocketPathTooLong = errors.New("httpapi: the unix socket path is too long")

// maxUnixSocketPath is the platform's sun_path capacity, including the
// terminating NUL. It is a fixed-size array in a C struct, not a path limit —
// which is why a perfectly ordinary data directory nested a few levels deep
// produces "bind: invalid argument" and no clue as to why.
func maxUnixSocketPath() int {
	switch runtime.GOOS {
	case "linux", "android":
		return 108
	case "windows":
		return 256
	default: // darwin and the BSDs
		return 104
	}
}

// listenUnix binds the local socket, clearing a socket left behind by a crashed
// process but never one something is still listening on.
func (s *Server) listenUnix(path string) (net.Listener, error) {
	if len(path) >= maxUnixSocketPath() {
		return nil, fmt.Errorf("%w: %s is %d bytes and the limit on this platform is %d — "+
			"set http.unix_socket to a shorter path",
			errSocketPathTooLong, path, len(path), maxUnixSocketPath())
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("httpapi: creating the directory for %s: %w", path, err)
	}
	if _, err := os.Stat(path); err == nil {
		// Something is there. Dialling tells us whether it is a live server or
		// the corpse of one — deleting it unconditionally would let a second
		// controller silently steal the socket from a running first, which is
		// far worse than refusing to start.
		conn, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("httpapi: %s is already served by another heyarr process", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("httpapi: removing the stale socket %s: %w", path, err)
		}
		s.log.Warn("removed a stale unix socket left by a previous run", "path", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("httpapi: checking %s: %w", path, err)
	}

	l, err := listenUnixRestricted(path)
	if err != nil {
		return nil, fmt.Errorf("httpapi: listening on %s: %w", path, err)
	}
	return l, nil
}
