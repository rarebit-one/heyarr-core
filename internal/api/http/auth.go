package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// IdentityFrom returns the authenticated caller, if the request passed through
// the authentication middleware.
func IdentityFrom(ctx context.Context) (auth.Identity, bool) {
	id, ok := ctx.Value(ctxKeyIdentity).(auth.Identity)
	return id, ok
}

// anonymousIdentity is what every caller is when authentication is disabled —
// which configuration and the server's own bind check together permit only on a
// loopback listener. It holds admin so that scope requirements behave
// identically with and without authentication, rather than the disabled mode
// taking a second, less-tested code path through every handler.
var anonymousIdentity = auth.Identity{
	Anonymous: true,
	Principal: auth.Principal{Kind: "service", Name: "anonymous"},
	Token:     auth.Token{Name: "anonymous", Scopes: []auth.Scope{auth.ScopeAdmin}},
}

// authenticate resolves the bearer token into an identity, or rejects the
// request. It is last in the middleware chain, so a rejection is still counted,
// logged and correlated like any other response.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.HTTP.Auth.Enabled {
			next.ServeHTTP(w, s.withIdentity(r, anonymousIdentity))
			return
		}

		raw, ok := bearerToken(r)
		if !ok {
			// No credential at all is not a failure worth a metric spike or a
			// log line of its own: it is what every first request from a
			// browser looks like. RequireScope turns it into a 401.
			next.ServeHTTP(w, r)
			return
		}

		id, err := s.verifier.Verify(r.Context(), raw)
		if err != nil {
			s.metrics.authFails.WithLabelValues(authFailureReason(err)).Inc()
			// The reason is logged; the client is told only that the
			// credential was rejected. "No such token" and "wrong secret" are
			// different facts, and handing an unauthorised caller the
			// difference is free reconnaissance.
			s.log.Warn("rejected a credential",
				"request_id", RequestIDFrom(r.Context()),
				"reason", authFailureReason(err),
				"path", r.URL.Path)
			Fail(w, r, problem.Unauthorized("the presented credential was rejected"))
			return
		}

		next.ServeHTTP(w, s.withIdentity(r, id))
	})
}

// withIdentity puts the identity where handlers read it, and also into the slot
// the access log holds.
func (s *Server) withIdentity(r *http.Request, id auth.Identity) *http.Request {
	if slot, ok := r.Context().Value(ctxKeyIdentitySlot).(*identitySlot); ok {
		resolved := id
		slot.identity = &resolved
	}
	return r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity, id))
}

// authFailureReason is the label put on the metric and the log line. It is a
// closed set of ours, never the error text, so cardinality stays bounded.
func authFailureReason(err error) string {
	switch {
	case errors.Is(err, auth.ErrMalformedToken):
		return "malformed"
	case errors.Is(err, auth.ErrNotFound):
		return "unknown"
	case errors.Is(err, auth.ErrRevoked):
		return "revoked"
	case errors.Is(err, auth.ErrExpired):
		return "expired"
	case errors.Is(err, auth.ErrBadSecret):
		return "bad_secret"
	default:
		return "error"
	}
}

// bearerToken extracts the credential from the Authorization header.
//
// The header is the only place a token is accepted. A ?token= query parameter
// would be convenient for a media player and would also write the credential
// into every proxy log, browser history and Referer header between here and the
// client — which is why the access log redacts query values but the API does
// not read credentials from there in the first place.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	return tok, tok != ""
}

// RequireScope rejects a request whose identity does not carry the required
// authority. Authority is ordered: admin implies write implies read.
//
// It is a separate middleware from authentication on purpose, and that is what
// makes the 401/403 distinction honest — no credential is "who are you?", an
// insufficient one is "not you". Routes declare what they need:
//
//	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/works", handler)
//
// The /api/v1 router applies RequireScope(auth.ScopeRead) to everything, so a
// mounted route that forgets to say anything is closed rather than open.
func RequireScope(want auth.Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := IdentityFrom(r.Context())
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="heyarr"`)
				Fail(w, r, problem.Unauthorized("this endpoint requires a bearer token"))
				return
			}
			// A peer is not an admin (ADR-0033).
			//
			// A peer certificate authenticates as that peer and authorises the
			// peer surface — reporting inventory, fetching a snapshot, reading
			// its jobs. Creating tokens, enrolling peers and changing policy
			// are the admin surface, and no peer credential may reach them: a
			// peer that could enrol a peer could enrol itself a second
			// identity, and one that could mint a token could stop being a
			// peer altogether.
			//
			// The refusal is here rather than in the admin handlers because
			// the handler that forgets is the one that matters, and here every
			// admin route inherits it — including the one added after this was
			// written.
			if want == auth.ScopeAdmin && PeerConnection(r.Context()) {
				Fail(w, r, problem.Forbidden(
					"a peer certificate is not an admin credential. It authenticates as that peer and "+
						"authorises the peer surface only (ADR-0033); the admin surface is reached with "+
						"an admin-scoped bearer token on a connection that is not a peer's"))
				return
			}
			if !id.Allows(want) {
				Fail(w, r, problem.Forbidden("this token does not carry the "+string(want)+" scope"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
