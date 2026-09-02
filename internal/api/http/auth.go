package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/voidbind-go/rp"
)

// DeviceVerifier authenticates a device credential (ADR-0048, ADR-0068): the
// device's admitting membership op plus a proof the caller holds the device
// key, evaluated offline against a pinned genesis key and the identity's op
// log merged with the ops the device presented (the Voidbind-Membership
// header). It is an interface so the server can be wired without the identity
// store in tests, mirroring PeerMembership.
type DeviceVerifier interface {
	Verify(ctx context.Context, credential string, presented []string, now time.Time) (deviceauth.Authenticated, error)
}

// presentedMembership reads the ops a device sent beside its credential
// (rp.MembershipHeader, ADR-0068) so a device admitted by a member this node
// has never met is judged with the evidence it carries. Absent is no ops; over
// rp.MaxPresentedOps is a refusal, not a truncation — a device that sends more
// than the cap is misbehaving, and silently dropping some of its ops could turn
// an authorised admission into an unjudgeable one.
func presentedMembership(r *http.Request) ([]string, error) {
	return rp.ParseMembershipHeader(r.Header.Get(rp.MembershipHeader))
}

// deviceCredential extracts the value presented under the "Device" scheme, the
// device counterpart to bearerToken. The header is the only place it is
// accepted, for the same reason a bearer token is: a query parameter writes the
// credential into every proxy log and Referer between here and the client.
func deviceCredential(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = deviceauth.Scheme + " "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	v := strings.TrimSpace(h[len(prefix):])
	return v, v != ""
}

// deviceFailureReason is the closed metric/log label set for a rejected device
// credential — never the error text, so cardinality stays bounded, and every
// distinct refusal is tellable apart in the log without being disclosed to the
// caller.
func deviceFailureReason(err error) string {
	switch {
	case errors.Is(err, deviceauth.ErrMalformedCredential), errors.Is(err, enrolment.ErrMalformed),
		errors.Is(err, deviceauth.ErrMalformedOp):
		return "device_malformed"
	case errors.Is(err, deviceauth.ErrUnknownUser):
		return "device_unknown_user"
	case errors.Is(err, deviceauth.ErrUnknownDevice):
		return "device_unknown_device"
	case errors.Is(err, deviceauth.ErrDeviceRevoked):
		return "device_revoked"
	case errors.Is(err, deviceauth.ErrCertMismatch):
		return "device_cert_mismatch"
	case errors.Is(err, rp.ErrRemoved):
		return "device_removed"
	case errors.Is(err, rp.ErrNotMember):
		return "device_not_member"
	case errors.Is(err, rp.ErrTooManyOps):
		return "device_too_many_ops"
	case errors.Is(err, enrolment.ErrExpired):
		return "device_cert_expired"
	case errors.Is(err, enrolment.ErrNotYetValid):
		return "device_cert_not_yet_valid"
	case errors.Is(err, enrolment.ErrBadSignature), errors.Is(err, enrolment.ErrUnknownUser):
		return "device_cert_bad_signature"
	case errors.Is(err, enrolment.ErrPossessionExpired):
		return "device_possession_expired"
	case errors.Is(err, enrolment.ErrPossessionNotYet):
		return "device_possession_not_yet_valid"
	case errors.Is(err, enrolment.ErrPossessionSignature):
		return "device_possession_bad_signature"
	case errors.Is(err, enrolment.ErrPossessionCert):
		return "device_possession_wrong_cert"
	default:
		return "device_error"
	}
}

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

		if raw, ok := bearerToken(r); ok {
			id, err := s.verifier.Verify(r.Context(), raw)
			if err == nil {
				next.ServeHTTP(w, s.withIdentity(r, id))
				return
			}
			// A Voidbind web-login session token is also carried as a Bearer
			// credential (ADR-0053). It is tried ONLY after the primary verifier
			// declines this value, so a real service token keeps its exact path,
			// metrics and error mapping, and only an otherwise-rejected bearer value
			// is ever offered to the broker. On a hit the browser or TV acts as the
			// pinned user its device approved for, at the READ floor — always. A
			// session is a replayable bearer token and never lifts to write; write
			// is a device-credential action (ADR-0065, subsuming ADR-0061).
			if s.sessions != nil {
				if p, ok := s.sessions.Session(raw); ok {
					next.ServeHTTP(w, s.withIdentity(r, sessionIdentity(p)))
					return
				}
			}
			s.rejectCredential(w, r, authFailureReason(err))
			return
		}

		// A device presents its identity under its own scheme (ADR-0048). It is
		// tried only when a verifier is wired: with no user enrolled there is
		// nothing to authenticate against, and a device credential falls through
		// to the 401 any unrecognised credential gets.
		if cred, ok := deviceCredential(r); ok && s.deviceV != nil {
			presented, err := presentedMembership(r)
			if err != nil {
				s.rejectCredential(w, r, deviceFailureReason(err))
				return
			}
			id, err := s.authenticateDevice(r.Context(), cred, presented)
			if err != nil {
				s.rejectCredential(w, r, deviceFailureReason(err))
				return
			}
			next.ServeHTTP(w, s.withIdentity(r, id))
			return
		}

		// No credential at all, or a scheme this server does not handle, is not
		// a failure worth a metric spike or a log line of its own: it is what
		// every first request from a browser looks like. RequireScope turns it
		// into a 401.
		next.ServeHTTP(w, r)
	})
}

// rejectCredential logs the real reason and tells the client only that the
// credential was rejected. Two different facts — "no such user" and "bad
// signature" — look identical to the caller, because handing an unauthorised
// one the difference is free reconnaissance. Same stance as the bearer path.
func (s *Server) rejectCredential(w http.ResponseWriter, r *http.Request, reason string) {
	s.metrics.authFails.WithLabelValues(reason).Inc()
	s.log.Warn("rejected a credential",
		"request_id", RequestIDFrom(r.Context()),
		"reason", reason,
		"path", r.URL.Path)
	Fail(w, r, problem.Unauthorized("the presented credential was rejected"))
}

// authenticateDevice resolves a device credential into the identity of the user
// it acts as. No token is issued: the identity is the device key and a
// user-signed cert, verified offline against a pinned key.
//
// The device holds the read floor by default and WRITE when its key is
// authorised (ADR-0065). This is the subsume: write is earned by a device that
// is both enrolled — proven by the credential verifying at all — and authorised
// by an admin, so it rests on a hardware-held key and a per-request possession
// proof rather than a replayable session token. The authorization lookup fails
// CLOSED: an error keeps the device on the read floor rather than opening it,
// the same stance the session lift took. Anything finer than write is a
// capability grant (internal/grant, #304), not a scope.
func (s *Server) authenticateDevice(ctx context.Context, credential string, presented []string) (auth.Identity, error) {
	a, err := s.deviceV.Verify(ctx, credential, presented, s.now())
	if err != nil {
		return auth.Identity{}, err
	}
	scopes := []auth.Scope{auth.ScopeRead}
	if s.managementAuthorized(ctx, a.DeviceKey) {
		scopes = []auth.Scope{auth.ScopeRead, auth.ScopeWrite}
	}
	return auth.Identity{
		Principal: auth.Principal{ID: a.PrincipalID, Kind: "user", Name: a.PrincipalName},
		Token: auth.Token{
			Name:        "device:" + a.DeviceKey,
			PrincipalID: a.PrincipalID,
			Scopes:      scopes,
		},
	}, nil
}

// managementAuthorized resolves whether a device key is authorised for write
// (ADR-0065, subsuming ADR-0061). It fails closed: with no authorizer wired, or
// on a lookup error, the device stays on the read floor. A blank device key is
// never authorised — only a real enrolled device can be.
func (s *Server) managementAuthorized(ctx context.Context, deviceKey string) bool {
	if s.mgmtAuth == nil || deviceKey == "" {
		return false
	}
	ok, err := s.mgmtAuth.ManagementAuthorized(ctx, deviceKey)
	if err != nil {
		s.log.Warn("could not resolve a device write authorization; keeping the device on the read floor",
			"error", err)
		return false
	}
	return ok
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
