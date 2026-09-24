package httpjson

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers/ratelimit"
)

func TestGetSendsTheSharedHeadersAndDecodes(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{"name":"ok"}`)
	}))
	defer srv.Close()

	c := Client{HTTP: srv.Client(), Service: "svc", MaxBody: 1 << 10, UserAgent: "heyarr-test/1"}
	var body struct{ Name string }
	if err := c.Get(t.Context(), srv.URL, "search", &body, Bearer("tok"), Header{Key: "Api-Key", Value: "k"}); err != nil {
		t.Fatal(err)
	}
	if body.Name != "ok" {
		t.Errorf("decoded %+v", body)
	}
	for k, want := range map[string]string{
		"Accept": "application/json", "User-Agent": "heyarr-test/1",
		"Authorization": "Bearer tok", "Api-Key": "k",
	} {
		if got.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.Get(k), want)
		}
	}
}

func TestPostSendsAJSONBody(t *testing.T) {
	var ct, sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c := Client{HTTP: srv.Client(), Service: "svc", MaxBody: 1 << 10}
	var body struct{}
	if err := c.Post(t.Context(), srv.URL, "login", []byte(`{"a":1}`), &body); err != nil {
		t.Fatal(err)
	}
	if ct != "application/json" || sent != `{"a":1}` {
		t.Errorf("content-type %q body %q", ct, sent)
	}
}

func TestErrorsNameTheServiceAndOp(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		max     int64
		want    string
		status  int
	}{
		{
			name:    "non-200",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
			max:     1 << 10, want: "svc: search returned HTTP 401", status: http.StatusUnauthorized,
		},
		{
			name:    "undecodable",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `not json`) },
			max:     1 << 10, want: "svc: decoding search response: ",
		},
		{
			// A body cut at MaxBody is not decodable: the cap is a hard bound.
			name:    "over the cap",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"name":"long"}`) },
			max:     4, want: "svc: decoding search response: ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c := Client{HTTP: srv.Client(), Service: "svc", MaxBody: tc.max}
			var body struct{ Name string }
			err := c.Get(t.Context(), srv.URL, "search", &body)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("error = %v, want prefix %q", err, tc.want)
			}
			var he *Error
			if gotHTTP := errors.As(err, &he); gotHTTP != (tc.status != 0) || (gotHTTP && he.Status != tc.status) {
				t.Errorf("errors.As(*Error) = %v (%+v), want status %d", gotHTTP, he, tc.status)
			}
		})
	}

	// A transport failure names the op too.
	c := Client{HTTP: http.DefaultClient, Service: "svc", MaxBody: 1}
	err := c.Get(t.Context(), "http://127.0.0.1:0", "health", &struct{}{})
	if err == nil || !strings.HasPrefix(err.Error(), "svc: health request failed: ") {
		t.Errorf("transport error = %v", err)
	}
}

// The limiter is waited on before the request is built, and its error is
// returned untouched so a cancelled job reads as cancelled.
func TestALimiterErrorIsReturnedAsIs(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := Client{
		HTTP: http.DefaultClient, Service: "svc", MaxBody: 1,
		Limiter: ratelimit.New(time.Hour, nil, nil),
	}
	if err := c.Get(ctx, "http://example.invalid", "search", &struct{}{}); !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() {
		t.Errorf("error = %v, want the limiter's context.Canceled unwrapped", err)
	}
}
