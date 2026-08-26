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
// identity.
//
// POST /peer/v1/inventory is M4-07: a peer reporting what its disk actually
// holds, which the controller folds into `replicas` — additions, and removals
// as `missing` rather than deleted rows, so a peer that lost bytes is visible
// rather than silently absent.
//
// GET /peer/v1/reachback is #186: whether this node can reach the peer that is
// asking. Replication needs traffic in both directions — inventory is pushed
// peer → controller and bytes are pulled destination → source — and a node
// cannot observe the return direction for itself, so it asks. `peers add`
// consults it to refuse a one-way pairing at enrolment rather than leaving it
// to surface as a silent reconciliation (ADR-0037).
//
// GET /peer/v1/blobs/{hash}/manifest is M5-05: the chunk manifest for those
// same bytes, on the same trust root and with no new credential. It is a
// description the destination acts on, never a negotiation the source takes
// part in, and it NEVER generates the manifest whose absence it reports —
// 404 is the answer, and the destination that gets one pulls whole. See
// manifests.go.
//
// GET|HEAD /peer/v1/blobs/{hash}/content is M4-09: the byte-carrying hop
// itself. It is the SAME handler the client API serves, on a different trust
// root — ADR-0013 called this out in advance — and it is the only route in
// this repository over which a replica travels. See blobs.go.
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
	"strings"
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

// IdentityPath is where a peer asks another peer who it thinks it is talking
// to. It is exported because it is also the cheapest thing on this surface,
// and therefore what the health probe dials (#184) — a path spelled out a
// second time in another package is a path that goes stale in one of them.
const IdentityPath = Prefix + "/identity"

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
	// Inventory records a peer's inventory report against the catalog (M4-07).
	//
	// Optional. A peer surface with no sink still MOUNTS the route — the
	// OpenAPI parity test walks this router and an unmounted route would be
	// documented and unserved — and refuses reports with 503. Making it
	// optional keeps `heyarr all` and the peer-surface tests constructible
	// without a database, which is the same reason Addr may be empty.
	Inventory InventorySink
	// Blobs serves blob content to a pinned peer (M4-09).
	//
	// Optional, for the same reason Inventory is: a peer surface with no
	// content store still MOUNTS the route — the OpenAPI parity test walks
	// this router and an unmounted route would be documented and unserved —
	// and answers 503. That keeps this package constructible without a CAS,
	// which is what the parity test's own fixture needs.
	Blobs BlobServer
	// Manifests reads stored chunk manifests for pinned peers (M5-05).
	//
	// Optional, for the reason Blobs is, and nil is the ordinary state of a
	// node with no catalogue behind its peer surface: the route is still
	// MOUNTED — the OpenAPI parity test walks this router — and answers 503,
	// which is the same answer the content route gives, because a node with
	// nothing to serve has nothing to describe either.
	//
	// It is a READ and can only be a read. Nothing reachable through it may
	// generate a manifest or enqueue a chunk_blob job: a GET that chunked a
	// 20 GB blob to answer would be a remote denial of service (ADR-0034).
	Manifests ManifestSource
	// Pieces reports what this node holds of a blob, whole or in part (M6,
	// ADR-0042). Nil for the same reason as Manifests and with the same
	// consequence: a node serving no content has no pieces to report either.
	//
	// A READ, always. Nothing reachable through it may compute anything — the
	// geometry is derivable from the size without touching a byte, so there is
	// nothing to compute even if somebody wanted to.
	Pieces PieceSource
	// WebSeedOnly makes this node serve whole blobs and take no part in
	// swarms (§27, ADR-0042, #266).
	//
	// Separate from Pieces being nil, and the distinction is the whole of
	// #266. A nil Pieces means "there is no content store behind this peer
	// surface" — the node serves no bytes of any kind. This means the content
	// store is present and serving, and the operator has said this node is a
	// WEB SEED. The two answer differently because a destination does
	// different things with them, and because a refusal that says the wrong
	// thing sends somebody to the wrong file.
	//
	// Stated NEGATIVELY so that the zero value is the ordinary node. The first
	// version of this was `ServesPieces bool`, which made every existing
	// caller — and every future one that forgot the field — silently a web
	// seed. Two peerapi tests caught it immediately; a caller added later
	// would not have been so lucky.
	WebSeedOnly bool
	Logger      *slog.Logger
	// Now is injected so certificate validity is testable (ADR-0017).
	Now func() time.Time
	// Liveness records that a peer was heard from (§31, M4-10, #184).
	//
	// Optional, and nil on a peer surface with no control plane behind it —
	// but a deployment that leaves it nil in production has a peer fabric
	// whose health can never move: see [Liveness].
	Liveness Liveness
	// ReturnPath probes whether this node can reach the peer that is asking
	// (#186, ADR-0037). Nil on a node with no membership table behind its
	// peer surface, and the route then answers `unknown` rather than
	// disappearing — see reachback.go for why that is not a 503.
	ReturnPath ReturnPathProber
	// Snapshots builds catalog snapshots for attached Full Peers (§52,
	// M4-13). Nil on a node with no catalogue — a peer rather than a
	// controller (ADR-0029) — and the route then refuses honestly rather than
	// disappearing, so "this node does not do that" and "this build does not
	// have that route" stay distinguishable.
	Snapshots SnapshotSource
	// ControlBackup holds control-plane backups pushed to this peer (§50,
	// M7-03, ADR-0046). Nil on a node that stores no backups for others — the
	// route is still mounted and answers 503, for the same reason the others
	// are.
	ControlBackup ControlBackupSink
	// Leases serves this peer's signed access leases for a sibling to cache
	// ahead of an outage (§54, ADR-0048, #285). Nil on a node that issues no
	// leases — the route is still mounted and answers 503, like the others.
	Leases LeaseSource
}

// Server is the peer listener.
type Server struct {
	log     *slog.Logger
	addr    string
	self    string
	members mtls.Membership
	tls     *tls.Config
	handler http.Handler

	// inventory is the control plane's writer for peer-reported replicas. Nil
	// on a node with no catalog behind its peer surface.
	inventory InventorySink
	// snapshots builds the catalog snapshot a Full Peer materialises (§52,
	// M4-13). Nil for the same reason and with the same consequence: the route
	// is mounted and refuses honestly.
	snapshots SnapshotSource
	// blobs serves bytes to a pinned peer. Nil on a node with no content store
	// behind its peer surface.
	blobs BlobServer
	// liveness is where an authenticated inbound peer request is recorded as
	// evidence that the caller is up (#184).
	liveness Liveness
	// returnPath probes the caller's own address, for the enrolment-time
	// reachability check (#186, ADR-0037).
	returnPath ReturnPathProber

	// manifests describes those bytes to a pinned peer (M5-05). Nil for the
	// same reason and with the same consequence. A read, always.
	manifests ManifestSource
	// pieces answers what this node holds of a blob, whole or in part. Nil
	// when there is no store behind the surface. A read, always.
	pieces PieceSource
	// webSeedOnly is set when this node serves whole blobs and no pieces
	// (#266). See Options.WebSeedOnly for why it is distinct from pieces
	// being nil, and why it is stated negatively.
	webSeedOnly bool
	// controlBackup holds control-plane backups pushed to this peer (§50,
	// M7-03). Nil on a node that stores none.
	controlBackup ControlBackupSink
	// leases serves this peer's access leases for a sibling to cache (§54,
	// #285). Nil on a node that issues none.
	leases LeaseSource

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

		inventory:     opts.Inventory,
		snapshots:     opts.Snapshots,
		blobs:         opts.Blobs,
		liveness:      opts.Liveness,
		returnPath:    opts.ReturnPath,
		manifests:     opts.Manifests,
		pieces:        opts.Pieces,
		webSeedOnly:   opts.WebSeedOnly,
		controlBackup: opts.ControlBackup,
		leases:        opts.Leases,
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
		r.Get(strings.TrimPrefix(IdentityPath, Prefix), s.handleIdentity)
		// The controller-attachment pair (ADR-0029, ADR-0033). Both answer
		// with the peer the CERTIFICATE proved; the POST additionally compares
		// the declaration the peer sent and refuses a mismatch.
		r.Get("/attachment", s.handleAttachment)
		r.Post("/attach", s.handleAttach)
		// The inventory report (M4-07). A peer tells the controller what is
		// on its disk; the controller records it against the peer the
		// CERTIFICATE proved. This is the first surface on which a peer
		// causes `replicas` rows to be written about a machine that is not
		// this one.
		r.Post("/inventory", s.handleInventory)
		// The catalog snapshot a Full Peer materialises for degraded
		// operation (§52, M4-13). It is a read, and the mirror image of the
		// inventory report above: there a peer tells the controller what its
		// disk holds, here the controller tells the peer what the catalogue
		// holds. Neither lets a peer write control state (ADR-0029).
		r.Get("/catalog/snapshot", s.handleCatalogSnapshot)
		// Byte serving (§21, ADR-0013, ADR-0030, M4-09). This is the hop
		// replication actually travels: the destination opens this connection,
		// reads these bytes and verifies them itself. The controller is not
		// on it, and there is no route anywhere that would put it there — no
		// redirect, no proxy, no "just for the first sync" path (§32).
		//
		// HEAD as well as GET, because ADR-0013's contract includes it and a
		// caller asking only for the length must not be served a body.
		// The return leg of a pairing (#186, ADR-0037). Every other route
		// here answers a question about the CALLER; this one answers a
		// question about the network BETWEEN the two, and it is the only
		// question a node cannot answer for itself. See reachback.go.
		r.Get("/reachback", s.handleReachback)
		r.Get("/blobs/{hash}/content", s.handleBlobContent)
		r.Head("/blobs/{hash}/content", s.handleBlobContent)
		// The chunk manifest for those same bytes (§20, ADR-0034, M5-05).
		// Beside the content route, INSIDE this same r.Use(requirePeerIdentity)
		// subtree, and that placement is load-bearing rather than tidy: a new
		// route on an existing listener is exactly where a mount gets attached
		// to the wrong middleware chain, and a manifest route reachable without
		// membership would describe this node's entire library to anyone who
		// completed a TCP connection.
		//
		// It is a DESCRIPTION, not a negotiation. There is no body, no
		// destination inventory, and no difference computed here — the
		// destination decides for itself what to do with what it is told, and
		// this node never learns what it holds (ADR-0030). And asking never
		// generates: see manifests.go.
		r.Get("/blobs/{hash}/manifest", s.handleBlobManifest)
		// Which pieces this node holds of a blob it may hold only in part
		// (M6, ADR-0042). A read, and the question that makes a swarm
		// possible at all — see pieces.go.
		r.Get("/blobs/{hash}/pieces", s.handleBlobPieces)
		// One piece of a blob this node holds whole OR in part — the
		// byte-carrying half of the swarm. See pieces.go for why this is a
		// route rather than a Range on the content one.
		r.Get("/blobs/{hash}/pieces/{index}", s.handleBlobPiece)
		// The control-plane backup a peer pushes of its OWN control plane
		// (§50, M7-03, ADR-0046). This is the ONE route on this surface that
		// moves controller state, and it moves it INBOUND — a peer pushing to
		// this node — which is the mirror image of the catalog snapshot above,
		// a controller→peer read. Two artefacts, opposite directions, so §50's
		// "it is NOT the catalog snapshot" holds by construction. POST stores;
		// GET reports what this node holds of the caller's control plane, the
		// fact the pusher checks its belief against.
		r.Post("/control-backup", s.handleControlBackupReceive)
		r.Get("/control-backup", s.handleControlBackupList)
		// A recovering node fetching its OWN backup that this node holds
		// (§51, M7-04). The generation is in the path; the source is the
		// certificate's, so a peer downloads only its own control plane.
		r.Get("/control-backup/{generation}", s.handleControlBackupDownload)
		// A sibling caching this peer's access leases ahead of an outage (§54,
		// ADR-0048, #285). GET only: leases are read from the issuer, never
		// pushed to it, and each token carries its own authority so serving
		// them to a member discloses nothing (see handleLeases).
		r.Get("/leases", s.handleLeases)
	})

	// There is no admin route on this router and there is not going to be one.
	// A peer certificate authorises the peer surface, acting as that peer
	// (ADR-0033); token management, peer enrolment and policy live on the
	// client API behind an admin-scoped bearer token, on a listener that never
	// asks for a client certificate.
	return r
}

// Liveness records that a peer was heard from (§31, M4-10, #184).
//
// # Why the peer surface records it at all
//
// internal/peer/health makes liveness OBSERVED rather than declared: it is
// derived from interactions that were going to happen anyway, and there is no
// "I am healthy" endpoint for a peer to assert into. The client API's
// membership guard has recorded it since M4-10.
//
// That left a hole the size of the whole fabric. A remote peer talks to THIS
// surface — it reports an inventory, it pulls a snapshot, it drags a blob
// across the wire — and it holds no bearer token, so it never touches the
// client API's guard at all. In the topology M4 actually builds, nothing
// observed the interaction that actually happens, and a remote peer's stored
// health never left `unknown` (#184). This is that observation, made on the
// surface where the conversation happens.
//
// It is a small interface declared here rather than an import of the health
// tracker, for the reason the client API's PeerLiveness is one: this package
// authenticates peers and must not acquire the control plane's storage in
// order to do it. internal/peer/health.Tracker satisfies it.
type Liveness interface {
	// Seen records that this public key's peer made a request. It must never
	// affect the outcome of that request.
	Seen(ctx context.Context, publicKey []byte) error
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
		// Admitted — and the admission is itself the evidence. A peer that
		// opened this connection, presented a certificate this node pinned and
		// is still a member is a peer that is up, observed as a side effect of
		// work it was doing anyway (§31, M4-10, #184).
		//
		// Deliberately AFTER the membership check, exactly as the client API's
		// guard does it: a key that is not a member is not a peer whose
		// liveness this system has any business recording.
		//
		// It runs on a context detached from the request's, because the fact is
		// true whether or not the caller stays to hear the answer — a peer that
		// disconnects mid-response was still up when it asked. A failure here
		// is logged and never surfaced: recording that somebody is alive must
		// not be able to fail their request.
		if s.liveness != nil {
			if err := s.liveness.Seen(context.WithoutCancel(r.Context()), pub); err != nil {
				s.log.Error("recording that a peer was seen failed",
					"request_id", httpapi.RequestIDFrom(r.Context()),
					"peer_id", peer.PeerID, "path", r.URL.Path, "error", err)
			}
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
	// Speaks is what the ANSWERING node's peer surface will actually do, so a
	// caller can tell a piece peer from a web seed before it asks either of
	// them for anything (§27, ADR-0042, #266).
	//
	// # Why here, and why it is not configuration echoed back
	//
	// ADR-0038 makes each peer authoritative for its own site, so what a peer
	// does is the peer's statement and nobody else's — and this route already
	// exists to make exactly that kind of statement over authenticated mTLS.
	// A field on the ASKING node's membership record would be an operator's
	// claim about a machine they are not, and it would never self-correct.
	//
	// It is derived from what this server was BUILT with rather than from what
	// was configured, in the spirit of ADR-0039's "proven by execution": the
	// list cannot say piece-exchange while the piece routes refuse, because
	// both read the same two fields. ADR-0039 needed durability and an expiry
	// because a worker has no inbound surface to be asked on; a peer is
	// defined by having one, so the answer is fetched fresh and cannot go
	// stale.
	//
	// Sorted, so a caller may compare it and a golden file may contain it.
	Speaks []string `json:"speaks"`
}

// What a peer surface can say it speaks.
//
// Strings rather than an enum on the wire, because this list is read by nodes
// of other versions: one that meets a capability it does not know must ignore
// it, and one that meets a missing capability must not conclude the peer is
// broken. Both of those are easier to get right against a list of names.
const (
	// SpeaksBlobContent is the ranged content route every serving node has had
	// since M4-09 — the byte channel a web seed IS (§27, §28, ADR-0013).
	SpeaksBlobContent = "blob-content"
	// SpeaksPieceExchange is the availability and piece routes (M6, ADR-0042).
	// Its ABSENCE is what makes a member a web seed rather than a piece peer.
	SpeaksPieceExchange = "piece-exchange"
)

// speaks is what this node will actually answer, derived rather than declared.
func (s *Server) speaks() []string {
	var out []string
	if s.blobs != nil {
		out = append(out, SpeaksBlobContent)
	}
	if s.pieces != nil && !s.webSeedOnly {
		out = append(out, SpeaksPieceExchange)
	}
	return out
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
		Speaks:    s.speaks(),
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
