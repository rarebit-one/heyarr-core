package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
)

// TestARestrictedTokenIsConfinedOnTheRealRouter walks the router this
// controller actually serves (ADR-0104): every route outside the restricted
// allow-list — media, catalog, MCP, admin, /system, /metrics, and whatever is
// added tomorrow — refuses an executor token with the restricted code, and every
// allow-list entry names a route that exists, so the list cannot drift from
// the API it opens.
func TestARestrictedTokenIsConfinedOnTheRealRouter(t *testing.T) {
	h := newPeerSurfaceHarness(t, func(*http.Request) ([]byte, bool) { return nil, false })
	created, err := h.tokens.CreateExecutor(context.Background(), "executor", nil)
	if err != nil {
		t.Fatal(err)
	}
	token := created.Secret
	// Warm the token (argon2id is paid once) and prove it works at all: the
	// space listing is on the allow-list.
	if status, body := h.do(t, http.MethodGet, httpapi.APIPrefix+"/spaces", token); status != http.StatusOK {
		t.Fatalf("the executor token cannot reach an allowed route (%d)\n%s", status, body)
	}

	allowed := map[string]bool{}
	for _, rt := range httpapi.RestrictedAllowList() {
		allowed[rt.Method+" "+rt.Pattern] = true
	}

	served := map[string]bool{}
	var closed []string
	err = chi.Walk(h.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = normalisePath(route)
		served[method+" "+route] = true
		if (strings.HasPrefix(route, httpapi.APIPrefix) || route == "/metrics") && !allowed[method+" "+route] {
			closed = append(closed, method+" "+route)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for entry := range allowed {
		if !served[entry] {
			t.Errorf("the restricted allow-list names %q, which this controller does not serve", entry)
		}
	}
	if len(closed) < 50 {
		t.Fatalf("only %d routes to check; the walk is not seeing the API", len(closed))
	}
	for _, route := range closed {
		method, path, _ := strings.Cut(route, " ")
		status, body := h.do(t, method, path, token)
		// A HEAD response has no body to carry the code; its status is the answer.
		coded := method == http.MethodHead || strings.Contains(body, httpapi.CodeRestrictedPrincipal)
		if status != http.StatusForbidden || !coded {
			t.Errorf("%s answered an executor token with %d, want 403 %s\n%s",
				route, status, httpapi.CodeRestrictedPrincipal, body)
		}
	}
}
