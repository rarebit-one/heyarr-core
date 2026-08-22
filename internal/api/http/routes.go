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
