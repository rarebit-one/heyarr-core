package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResolveRenderer covers the naming logic, which is the whole interface: a
// UDN is unsayable, so everything is matched on what the device calls itself.
//
// The ambiguity case is the one with a consequence. This household has two
// televisions, and resolving "samsung" to the wrong one puts a film on a
// screen in a room nobody is in — a failure the person standing in the other
// room cannot see and will not understand. So it is an error listing the
// candidates, never a guess.
func TestResolveRenderer(t *testing.T) {
	t.Parallel()

	known := []Renderer{
		{UDN: "uuid:aaa", Name: "Samsung QN85BA 55", Model: "QA55QN85BAKXXS"},
		{UDN: "uuid:bbb", Name: "Samsung S90CA 77", Model: "QA77S90CAKXXS"},
		{UDN: "uuid:ccc", Name: "Phantom II 95 dB-a98d", Model: "Phantom II 95 dB"},
	}

	tests := []struct {
		name    string
		known   []Renderer
		want    string
		wantUDN string
		wantErr []string
	}{
		{name: "an exact UDN", known: known, want: "uuid:bbb", wantUDN: "uuid:bbb"},
		{name: "a distinctive substring", known: known, want: "Phantom", wantUDN: "uuid:ccc"},
		{name: "case does not matter", known: known, want: "phantom", wantUDN: "uuid:ccc"},
		{name: "a model number", known: known, want: "QN85BA", wantUDN: "uuid:aaa"},
		{name: "surrounding space", known: known, want: "  Phantom  ", wantUDN: "uuid:ccc"},
		{
			// Two televisions, one word.
			name: "ambiguous names both candidates", known: known, want: "Samsung",
			wantErr: []string{"more than one", "QN85BA", "S90CA"},
		},
		{
			name: "no match lists what is known", known: known, want: "kitchen",
			wantErr: []string{"no device matches", "Phantom"},
		},
		{
			// An empty list must not read as "that name is wrong".
			name: "nothing was discovered", known: nil, want: "living room",
			wantErr: []string{"nothing answered", "switched off"},
		},
		{name: "empty input", known: known, want: "", wantErr: []string{"say which device"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(RendererList{Renderers: tc.known})
			}))
			defer srv.Close()

			c, err := New(Options{Addr: srv.URL, Token: "t"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.ResolveRenderer(context.Background(), tc.want)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("ResolveRenderer(%q) = %+v, want an error", tc.want, got)
				}
				for _, frag := range tc.wantErr {
					if !strings.Contains(err.Error(), frag) {
						t.Errorf("error %q does not mention %q", err, frag)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveRenderer(%q): %v", tc.want, err)
			}
			if got.UDN != tc.wantUDN {
				t.Errorf("resolved to %s (%s), want %s", got.UDN, got.Name, tc.wantUDN)
			}
		})
	}
}

// TestResolveRendererDoesNotRefresh guards a small kindness: resolving a name
// must not trigger a network search. Discovery takes seconds, and every
// pause would pay for it.
func TestResolveRendererDoesNotRefresh(t *testing.T) {
	t.Parallel()

	var sawRefresh bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("refresh") == "true" {
			sawRefresh = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RendererList{
			Renderers: []Renderer{{UDN: "uuid:aaa", Name: "Samsung"}},
		})
	}))
	defer srv.Close()

	c, err := New(Options{Addr: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResolveRenderer(context.Background(), "samsung"); err != nil {
		t.Fatal(err)
	}
	if sawRefresh {
		t.Error("resolving a name asked the server to search the network again")
	}
}
