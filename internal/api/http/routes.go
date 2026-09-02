package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// APIPrefix is where the JSON API is mounted (spec §77).
const APIPrefix = "/api/v1"

// RenderPrefix is where capability-addressed content is mounted
// (ADR-0040). It is deliberately NOT under APIPrefix: everything there
// requires a bearer credential, and the whole point of this route is
// serving a device that has none to give.
//
// The capability is in the PATH, so the access log redacts it — see
// logPath. A secret in a URL is the thing auth.go objects to, and the
// mitigation it names for query strings has to apply here too.
const RenderPrefix = "/render"

// RelayPrefix is where the device-pairing relay is mounted (§40, ADR-0022,
// ADR-0038). Like RenderPrefix it is deliberately OUTSIDE APIPrefix and its
// authenticated group: a device being paired is not yet enrolled and has no
// credential to present, and the relay is a DUMB store-and-forward of PUBLIC
// values (two commitments, two public keys, a salt, a signed cert). It learns no
// key material and vouches for nothing, which is why serving it without a
// credential adds no authority to anyone — it is a rendezvous, not a resource.
const RelayPrefix = "/pair"

// RelayV1Prefix is where the Voidbind relay — voidbind-go's relay.Server, the
// protocol the voidbind CLI and the phone (voidbind-kmp) speak — is mounted
// (ADR-0066). It sits beside the legacy relay above, not in place of it. A
// Voidbind client is given "<node>/pair" (RelayPrefix) as its relay BASE — the
// client appends the /v1/... paths itself, as it would against a standalone
// `voidbind relay` — and so lands here. Public for the same reason RelayPrefix
// is.
const RelayV1Prefix = RelayPrefix + "/v1"

// EnrolPath is where a paired device enrols itself (ADR-0067): POST {cert,
// proof, name}. Like RelayPrefix it is deliberately OUTSIDE APIPrefix and its
// authenticated group — the device presenting the cert is, by definition, not
// yet enrolled, so the Device scheme would refuse it — and it grants nothing the
// cert does not already prove: the device authenticates afterwards at the read
// floor, and write stays an admin authorisation (ADR-0065).
const EnrolPath = "/enrol"

// MembershipPrefix is where an identity's membership op log is read and pushed
// (ADR-0068): GET /membership/{usr} returns the ops this node holds, POST
// /membership/{usr} records ops a device pushes (a remove, typically). Public
// for the same reason EnrolPath is: an op is self-authenticating — signed by a
// member of a pinned identity, evaluated before it is recorded — and the
// device pushing a remove may be the one that no longer authenticates.
const MembershipPrefix = "/membership"

// routes builds the whole handler.
//
// The middleware order is the design, not an accident:
//
//	request id  — so everything after it can name the request
//	access log  — so a request that dies further in is still logged
//	recovery    — inside the log, so a panic is logged as a 500 rather than
//	              vanishing, and outside the handler so it cannot escape
//	metrics     — before auth, so rejected credentials show up on the graph
//	auth        — last, so a rejection is a normal logged, counted response
func (s *Server) routes(mounts []MountFunc) http.Handler {
	r := chi.NewRouter()
	r.Use(
		requestIDMiddleware,
		// nosniff early, so it is set even on a response written by the panic
		// recovery — which is exactly the path least likely to have remembered.
		nosniffMiddleware,
		s.accessLogMiddleware,
		s.recoveryMiddleware,
		s.metricsMiddleware,
	)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		Fail(w, r, problem.NotFound("no route matches "+r.URL.Path))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		Fail(w, r, problem.New(http.StatusMethodNotAllowed, problem.TypeBadRequest,
			"Method Not Allowed", r.Method+" is not allowed on "+r.URL.Path))
	})

	// Liveness and readiness are unauthenticated by necessity: an orchestrator
	// probing them has no credential, and a health check that needs a secret is
	// a health check that gets disabled. They are kept free of anything an
	// unauthenticated caller should not learn — no paths, no versions, no
	// counts. "ready" or "not ready", and which subsystem, is all they say.
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	// /metrics is authenticated exactly like the API, and requires `read`.
	//
	// The rule stated plainly: whenever authentication is enabled, /metrics
	// needs a token; when it is disabled, /metrics is open — and authentication
	// can only be disabled on a loopback listener, which the server refuses to
	// start without. So there is no configuration in which /metrics is reachable
	// unauthenticated from off the machine. Making it loopback-only instead was
	// the alternative, and it fails the actual deployment: Prometheus scrapes
	// from another host, and an endpoint that leaks route names, library sizes
	// and request patterns is not "just metrics".
	r.Group(func(r chi.Router) {
		r.Use(s.authenticate, RequireScope(auth.ScopeRead))
		r.Method(http.MethodGet, "/metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{
			ErrorLog:          slogErrorLog{s.log},
			ErrorHandling:     promhttp.ContinueOnError,
			EnableOpenMetrics: true,
		}))
	})

	// Capability-addressed content (ADR-0040), before the authenticated group
	// and deliberately outside it.
	//
	// A renderer cannot present a credential, so the unguessable, expiring,
	// single-blob capability in the path is the authority. Mounting it here
	// rather than under APIPrefix is the whole design: inside that group the
	// route would 401 every television it exists to serve.
	for _, mount := range s.publicMounts {
		mount(r)
	}

	r.Route(APIPrefix, func(r chi.Router) {
		// Deny by default. Every route under /api/v1 needs at least `read`, so
		// a mounted route that forgets to declare a scope is closed rather than
		// wide open — the failure mode of the opposite default is silent.
		r.Use(s.authenticate, RequireScope(auth.ScopeRead))

		// Membership, after the bearer credential and before every route.
		//
		// It is here rather than inside the handlers that serve bytes because
		// ADR-0012 makes membership the ONLY trust root in the inter-peer
		// path: a peer whose record was removed must lose the whole API, on
		// the connection it is already holding open, not just the endpoint
		// somebody remembered to guard. Mounted centrally, a route added
		// tomorrow inherits it.
		if s.peers != nil {
			r.Use(peerMembershipGuard(s.peers, s.peerLiveness, s.presentedKey, s.log))
		}

		r.Get("/system", s.handleSystem)

		for _, mount := range mounts {
			mount(r)
		}
	})

	return r
}

// slogErrorLog adapts slog to the promhttp.Logger interface.
type slogErrorLog struct {
	log interface{ Error(string, ...any) }
}

func (l slogErrorLog) Println(v ...any) { l.log.Error("metrics handler", "error", v) }

// writeJSON renders a successful JSON response. Errors are never written this
// way — those are problem documents.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		s.log.Error("encoding a response failed",
			"request_id", RequestIDFrom(r.Context()), "path", r.URL.Path, "error", err)
		Fail(w, r, problem.Internal())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
