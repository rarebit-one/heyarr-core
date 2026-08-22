// Package peerapi serves the peer fabric, and only the peer fabric (§26,
// ADR-0012, M4-05).
//
// # Why this is a second listener rather than a route group
//
// The client API authenticates a bearer token (ADR-0011). This surface
// authenticates a pinned Ed25519 key in a client certificate. Those are two
// trust roots, and the requirement is not merely that each route picks the
// right one — it is that a credential for one CANNOT be presented to the
// other:
//
//   - a bearer token must not authenticate a peer request, and here it cannot,
//     because nothing in this package reads Authorization and the TLS listener
//     refuses a connection that presents no client certificate before any
//     header is parsed;
//   - a peer certificate must not authenticate a client request, and there it
//     cannot, because the client API's peer guard only ever SUBTRACTS: a
//     presented peer key gets a request refused, never authenticated. The
//     bearer credential is still required.
//
// A single router with two middleware chains would put both credentials in
// scope on every request and leave "which one applies here" to a per-route
// decision — and the failure mode of a per-route decision is one route that
// forgot. Two listeners make it structural: a bearer token sent here reaches a
// handler that has never heard of tokens, and a client certificate sent to the
// client API reaches a guard that can only refuse.
//
// # What it serves
//
// GET /peer/v1/identity reports the peer identity this node DERIVED FROM THE
// PRESENTED CERTIFICATE. It exists because "the request returned 200" is not
// evidence that pinning happened — a server that authenticated nobody would
// also return 200 — and the acceptance condition M4-05 owes is that the id the
// server derived equals the id enrolment returned.
//
// GET /peer/v1/attachment and POST /peer/v1/attach are the peer → controller
// link (ADR-0029, ADR-0033): a controller-attached Full Peer confirms which
// controller it is attached to and what that controller records it as. The
// POST carries a DECLARED peer id, which is compared against the certificate
// identity and refused on a mismatch — it is never read as the acting
// identity. Byte serving over this surface is M4-07 and M4-09.
//
// # A peer is not an admin
//
// Everything mounted here authorises as the calling peer, acting on its own
// resources. There is no route here that mints a token, enrols a peer or
// changes policy, and there will not be: those are the admin surface, on the
// other listener, behind an admin-scoped bearer token (ADR-0033). The
// symmetric half of that rule lives in httpapi.RequireScope, which refuses an
// admin route to a request arriving over a peer certificate.
package peerapi

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// Prefix is where the peer fabric's routes live. It is deliberately not
// /api/v1: the two surfaces are different contracts with different
// credentials, and a shared prefix would invite a future route onto whichever
// one it was mounted on by accident.
const Prefix = "/peer/v1"

// readHeaderTimeout bounds how long a peer may take to send its headers.
const readHeaderTimeout = 20 * time.Second

// Options configure a peer Server.
type Options struct {
	// Addr is the TCP address to bind. Empty means this node serves no peer
	// surface, which is the correct state for a single-node deployment and the
	// default: `heyarr all` must keep working with no certificate
	// configuration at all.
	Addr string
	// Material is this node's certificate, derived from its Ed25519 identity.
	Material *mtls.Material
	// Members is the trust root, consulted per connection AND per request.
	Members mtls.Membership
	// SelfPeerID is this node's own membership id, reported so a peer can tell
	// which node answered.
	SelfPeerID string
	Logger     *slog.Logger
	// Now is injected so certificate validity is testable (ADR-0017).
	Now func() time.Time
}

// Server is the peer listener.
type Server struct {
	log     *slog.Logger
	addr    string
	self    string
	members mtls.Membership
	tls     *tls.Config
	handler http.Handler

	http     *http.Server
	bound    string
	errc     chan error
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// New builds the peer server and its TLS configuration. It binds nothing;
// Start does that, so a configuration failure is reported before any socket
// exists.
func New(opts Options) (*Server, error) {
	if opts.Material == nil {
		return nil, errors.New("peerapi: the peer surface needs this node's certificate material")
	}
	if opts.Members == nil {
		return nil, errors.New("peerapi: the peer surface needs a membership trust root — " +
			"without one it would authenticate every key that connects (ADR-0012)")
	}
	if opts.SelfPeerID == "" {
		return nil, errors.New("peerapi: the peer surface needs this node's peer id")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "peerapi")

	tlsCfg, err := mtls.ServerConfig(mtls.Options{
		Material: opts.Material,
		Members:  opts.Members,
		Now:      opts.Now,
		Logger:   log,
	})
	if err != nil {
		return nil, fmt.Errorf("peerapi: %w", err)
	}

	s := &Server{
		log:     log,
		addr:    opts.Addr,
		self:    opts.SelfPeerID,
		members: opts.Members,
		tls:     tlsCfg,
		errc:    make(chan error, 1),
	}
	s.handler = s.routes()
	s.http = &http.Server{
		Handler:           s.handler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// Handler exposes the router so tests can walk it — the OpenAPI parity test
// (ADR-0015) enumerates this surface as well as the client API's.
func (s *Server) Handler() http.Handler { return s.handler }

// TLSConfig exposes the pinned configuration this listener serves with.
func (s *Server) TLSConfig() *tls.Config { return s.tls }

// routes builds the peer router.
//
// There is no authenticate middleware and no scope floor, because there are no
// tokens on this surface. The credential is the connection, and it was
// verified before this router was reached.
func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpapi.Fail(w, r, problem.NotFound("no peer route matches "+r.URL.Path))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpapi.Fail(w, r, problem.New(http.StatusMethodNotAllowed, problem.TypeBadRequest,
			"Method Not Allowed", r.Method+" is not allowed on "+r.URL.Path))
	})
	r.Route(Prefix, func(r chi.Router) {
		r.Use(s.requirePeerIdentity)
		r.Get("/identity", s.handleIdentity)
		// The controller-attachment pair (ADR-0029, ADR-0033). Both answer
		// with the peer the CERTIFICATE proved; the POST additionally compares
		// the declaration the peer sent and refuses a mismatch.
		r.Get("/attachment", s.handleAttachment)
		r.Post("/attach", s.handleAttach)
	})

	// There is no admin route on this router and there is not going to be one.
	// A peer certificate authorises the peer surface, acting as that peer
	// (ADR-0033); token management, peer enrolment and policy live on the
	// client API behind an admin-scoped bearer token, on a listener that never
	// asks for a client certificate.
	return r
}

// peerContextKey carries the member a request's certificate proved.
type peerContextKey struct{}

// PeerFrom reports the peer a request authenticated as. The second result is
// false on any request that did not reach a peer route.
func PeerFrom(ctx context.Context) (mtls.Peer, bool) {
	p, ok := ctx.Value(peerContextKey{}).(mtls.Peer)
	return p, ok
}

// requirePeerIdentity re-derives the peer from the certificate the handshake
// verified, and consults membership again.
//
// "Again" is the point. The handshake already pinned this key, and that answer
// is now as old as the connection — which, with keep-alives, is as old as the
// operator's patience. ADR-0012 leaves no CRL, so a removed peer must lose the
// connection it is already holding open, and the only thing that achieves that
// is asking on every request (M4-04). This is the same check the client API's
// guard makes, deliberately duplicated rather than shared, because the two
// surfaces must be able to refuse independently.
func (s *Server) requirePeerIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pub, isPeer := httpapi.TLSPresentedPeerKey(r)
		if !isPeer {
			// Unreachable behind the listener, which requires a client
			// certificate — and present anyway, because this handler is also
			// reachable through Handler() in a test, and a surface whose
			// authentication lives entirely in its listener is a surface that
			// authenticates nothing the day somebody mounts it elsewhere.
			httpapi.Fail(w, r, problem.Unauthorized(
				"the peer fabric authenticates by client certificate only; no certificate was presented. "+
					"A bearer token is not a peer credential (ADR-0011, ADR-0012)"))
			return
		}
		peer, err := s.members.Lookup(r.Context(), pub)
		switch {
		case errors.Is(err, mtls.ErrNotAMember):
			httpapi.Fail(w, r, problem.Forbidden(
				"this peer is not a member of this fabric. Membership is the only trust root "+
					"in the inter-peer path (ADR-0012), and it is consulted on every request — "+
					"a removed peer loses access on the connection it is already using"))
			return
		case err != nil:
			// Fail closed: an unavailable trust root is not permission.
			s.log.Error("checking peer membership failed",
				"request_id", httpapi.RequestIDFrom(r.Context()), "path", r.URL.Path, "error", err)
			httpapi.Fail(w, r, problem.Internal())
			return
		}
		if !ed25519.PublicKey(peer.PublicKey).Equal(ed25519.PublicKey(pub)) {
			httpapi.Fail(w, r, problem.Forbidden(
				"the membership record returned does not pin the key this connection presented"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerContextKey{}, peer)))
	})
}

// IdentityResponse is what GET /peer/v1/identity answers.
//
// peer_id is the identity THIS NODE DERIVED from the certificate the caller
// presented — not something the caller asserted, and not an echo of anything
// in the request. That distinction is the whole value of the endpoint: it is
// the only way an acceptance assertion can say "the server knows who I am"
// rather than "the server did not say no".
type IdentityResponse struct {
	// PeerID is the caller's membership id, derived from its certificate.
	PeerID string `json:"peer_id"`
	// Name is the caller's name in this node's membership records.
	Name string `json:"name"`
	// PublicKey is the pinned key, rendered as ed25519:<64 hex>, so a caller
	// can compare it against what it enrolled.
	PublicKey string `json:"public_key"`
	// ServedBy is the answering node's own peer id, so a caller can tell which
	// end of the fabric replied.
	ServedBy string `json:"served_by"`
}

func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	peer, ok := PeerFrom(r.Context())
	if !ok {
		// The middleware above is the only path here, so this is a wiring
		// failure rather than a request failure.
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	body := IdentityResponse{
		PeerID:    peer.PeerID,
		Name:      peer.Name,
		PublicKey: identity.FormatPublicKey(peer.PublicKey),
		ServedBy:  s.self,
	}
	s.writeJSON(w, r, body)
}

// writeJSON renders a successful peer response. Errors never come through here
// — those are problem documents, written by httpapi.Fail.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		s.log.Error("encoding a peer response failed",
			"request_id", httpapi.RequestIDFrom(r.Context()), "path", r.URL.Path, "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// Start binds the peer listener and serves in the background. A Server with no
// address configured starts nothing and reports no error: a deployment with
// one peer has no fabric to serve.
func (s *Server) Start() error {
	if s.addr == "" {
		return nil
	}
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("peerapi: listening on %s: %w", s.addr, err)
	}
	s.bound = l.Addr().String()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// ServeTLS with empty file arguments uses s.http.TLSConfig, which is
		// the pinned configuration. There is no certificate file and there
		// never will be: the certificate is derived from the identity in
		// memory (ADR-0012).
		if err := s.http.ServeTLS(l, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case s.errc <- fmt.Errorf("peerapi: serving on %s: %w", s.bound, err):
			default:
			}
		}
	}()
	s.log.Info("peer surface listening", "addr", s.bound, "peer_id", s.self)
	return nil
}

// Err delivers the first fatal serving error.
func (s *Server) Err() <-chan error { return s.errc }

// Addr is the address actually bound, or empty when the peer surface is off.
func (s *Server) Addr() string { return s.bound }

// Shutdown stops accepting and drains in-flight peer requests.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		if s.bound == "" {
			return
		}
		err = s.http.Shutdown(ctx)
		s.wg.Wait()
	})
	return err
}
