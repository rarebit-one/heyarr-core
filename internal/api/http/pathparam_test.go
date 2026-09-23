package httpapi

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestHashParam covers the reason the helper exists: a content-address carries a
// ':' and clients disagree on whether to percent-encode it. Both spellings must
// resolve to the same literal `blake3:<hex>` that hashing.Parse and the blobs
// table expect.
func TestHashParam(t *testing.T) {
	const literal = "blake3:6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"

	cases := []struct {
		name string
		raw  string // exactly what chi.URLParam would return from the routed target
		want string
	}{
		{"literal colon (Go net/http client, peer, curl)", literal, literal},
		{"encoded colon (JVM java.net.http, RFC 3986 §2.1)", "blake3%3A6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85", literal},
		{"uppercase encoding is equivalent", "blake3%3a6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85", "blake3:6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"},
		{"malformed escape falls back to the raw value", "blake3%zz", "blake3%zz"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			rc := chi.NewRouteContext()
			rc.URLParams.Add("hash", tc.raw)
			r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))

			if got := HashParam(r); got != tc.want {
				t.Fatalf("HashParam(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
