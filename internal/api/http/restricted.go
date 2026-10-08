package httpapi

// restricted.go confines a restricted principal (ADR-0104) — an executor that
// works on an owner's behalf against a few encrypted spaces — to the vault
// surface. Deny is the default: the confinement is mounted at the root of the
// authenticated router, and only the routes named in restrictedAllowList are
// reachable. Within those, the handlers apply the per-space grant; this file
// decides only which ROUTES a restricted identity may knock on at all.
//
// An identity that is not restricted passes through untouched, so every
// existing token, device and session behaves exactly as it did before.

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// CodeRestrictedPrincipal is the stable code on every refusal a restricted
// principal gets for reaching outside its surface, so a client branches on the
// code rather than on the route it happened to hit.
const CodeRestrictedPrincipal = "restricted_principal"

// RestrictedRoute is one (method, pattern) a restricted principal may reach.
// Patterns are chi patterns under APIPrefix.
type RestrictedRoute struct {
	Method  string
	Pattern string
}

// restrictedAllowList is the WHOLE of what a restricted principal may reach
// (ADR-0104): reading and pushing the ciphertext of the spaces it is granted,
// and reading and storing vault blobs. Rotation, compaction, key deletion,
// re-wrap, space creation, replication, the grant API itself, media, the
// catalog, MCP, admin, /system and /metrics are absent, and therefore closed.
var restrictedAllowList = []RestrictedRoute{
	{http.MethodGet, APIPrefix + "/spaces"},
	{http.MethodGet, APIPrefix + "/spaces/{id}/keys"},
	{http.MethodGet, APIPrefix + "/spaces/{id}/key-history"},
	{http.MethodGet, APIPrefix + "/spaces/{id}/changes"},
	{http.MethodGet, APIPrefix + "/spaces/{id}/snapshot"},
	{http.MethodPost, APIPrefix + "/spaces/{id}/changes"},
	{http.MethodPost, APIPrefix + "/spaces/{id}/snapshots"},
	{http.MethodGet, APIPrefix + "/blobs/{hash}/content"},
	{http.MethodHead, APIPrefix + "/blobs/{hash}/content"},
	{http.MethodPut, APIPrefix + "/vault/blobs/{hash}"},
	{http.MethodPost, APIPrefix + "/vault/placements"},
}

// RestrictedAllowList returns a copy of the allow-list, for the test that walks
// the real router and asserts every entry names a route that exists — an entry
// that matches nothing is an allow-list that has drifted from the API.
func RestrictedAllowList() []RestrictedRoute {
	return append([]RestrictedRoute(nil), restrictedAllowList...)
}

// restrictedRoutes is the allow-list as a router, so matching uses chi's own
// pattern semantics rather than a second, hand-rolled matcher.
var restrictedRoutes = sync.OnceValue(func() chi.Routes {
	m := chi.NewMux()
	noop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, rt := range restrictedAllowList {
		m.Method(rt.Method, rt.Pattern, noop)
	}
	return m
})

// confineRestricted refuses a restricted identity any route outside the
// allow-list, with a 403 carrying CodeRestrictedPrincipal. Every other caller
// passes through.
func confineRestricted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFrom(r.Context())
		if !ok || !id.Restricted {
			next.ServeHTTP(w, r)
			return
		}
		if restrictedRoutes().Match(chi.NewRouteContext(), r.Method, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		Fail(w, r, restrictedRefusal())
	})
}

func restrictedRefusal() *problem.Problem {
	return problem.Forbidden(
		"this credential is restricted to the vault surface and the encrypted spaces it is granted (ADR-0104)").
		WithCode(CodeRestrictedPrincipal)
}

// RefuseRestricted rejects a restricted identity with 403 and passes every other
// caller through. The root confinement already keeps a restricted principal off
// every route not on the allow-list; a route that must NEVER be reachable by one
// — rotation, compaction, key deletion — says so here as well, so a later
// widening of the allow-list cannot open it by accident.
func RefuseRestricted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := IdentityFrom(r.Context()); ok && id.Restricted {
			Fail(w, r, restrictedRefusal())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SpaceWriteGrants answers whether a principal holds a write grant on at least
// one encrypted space (ADR-0104). A restricted principal must, to store a vault
// blob or record a placement: a blob carries no space (ADR-0096), so "some
// space it may write" is the strongest check the route can make.
// *personalstate/store.Store satisfies it.
type SpaceWriteGrants interface {
	HasAnyWriteGrant(ctx context.Context, principalID string) (bool, error)
}

// RequireRestrictedWriteGrant lets a restricted identity through only when g
// says its principal holds a write grant on some space. A nil g refuses every
// restricted identity (fail closed). Every other caller passes through.
func RequireRestrictedWriteGrant(g SpaceWriteGrants) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := IdentityFrom(r.Context())
			if !ok || !id.Restricted {
				next.ServeHTTP(w, r)
				return
			}
			if g == nil {
				Fail(w, r, restrictedRefusal())
				return
			}
			has, err := g.HasAnyWriteGrant(r.Context(), id.Principal.ID)
			if err != nil {
				Fail(w, r, problem.Internal())
				return
			}
			if !has {
				Fail(w, r, problem.Forbidden(
					"a restricted credential stores vault content only while it holds a write grant on a space (ADR-0104)").
					WithCode(CodeRestrictedPrincipal))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimiter is a fixed-window request budget per principal. It bounds how
// fast a restricted principal can probe blob hashes (ADR-0104): a hash is the
// capability, and a budget keeps a guessing attack from being a loop.
type RateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	start  time.Time
	counts map[string]int
}

// NewRateLimiter allows limit requests per principal per window. now is the
// injected clock (ADR-0017); nil means the system clock.
func NewRateLimiter(limit int, window time.Duration, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{limit: limit, window: window, now: now, counts: map[string]int{}}
}

// Allow spends one request of key's budget, reporting false when the window's
// budget is gone. The whole map resets when the window rolls over, so it holds
// at most one window's worth of principals.
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.start) >= l.window {
		l.start = now
		clear(l.counts)
	}
	if l.counts[key] >= l.limit {
		return false
	}
	l.counts[key]++
	return true
}

// TooManyRequests is the refusal a spent budget gets.
func TooManyRequests(w http.ResponseWriter, r *http.Request, retryAfter time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	Fail(w, r, problem.New(http.StatusTooManyRequests, problem.TypeBadRequest,
		"Too Many Requests", "this credential has spent its request budget for now; retry later").
		WithCode(CodeRestrictedPrincipal))
}
