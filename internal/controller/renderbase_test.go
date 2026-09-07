package controller

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/config"
)

// The public origin (ADR-0072) is the login/session rp origin and the rendered
// base URL when set: it beats both a derivable listener address and a configured
// peer endpoint, and it is returned verbatim (trailing slash trimmed) rather
// than reduced to an IP:port a browser cannot use as an origin.
func TestRenderBaseURLHonoursThePublicOrigin(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*config.Config)
		want string
	}{
		{
			name: "public origin beats a derived listener address",
			mut: func(c *config.Config) {
				c.HTTP.Addr = "192.168.16.5:7777"
				c.HTTP.PublicOrigin = "https://heyarr.br.example.com"
			},
			want: "https://heyarr.br.example.com",
		},
		{
			name: "public origin beats a configured peer endpoint",
			mut: func(c *config.Config) {
				c.Peer.Endpoint = "http://192.168.16.5:7777"
				c.HTTP.PublicOrigin = "https://heyarr.br.example.com"
			},
			want: "https://heyarr.br.example.com",
		},
		{
			name: "a trailing slash is trimmed",
			mut: func(c *config.Config) {
				c.HTTP.PublicOrigin = "https://heyarr.br.example.com/"
			},
			want: "https://heyarr.br.example.com",
		},
		{
			name: "unset falls back to the derived listener address",
			mut: func(c *config.Config) {
				c.HTTP.Addr = "192.168.16.5:7777"
			},
			want: "http://192.168.16.5:7777",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Defaults()
			tt.mut(&cfg)
			if got := renderBaseURL(cfg); got != tt.want {
				t.Errorf("renderBaseURL = %q, want %q", got, tt.want)
			}
		})
	}
}

// A plain-HTTP render listener (ADR-0079) is what RENDERERS are handed, and only
// them: the login/session origin browsers use is still the public origin. A DLNA
// renderer fetches without TLS, so the https origin is one it cannot play from.
func TestRendererBaseURLPrefersThePlainRenderListener(t *testing.T) {
	cfg := config.Defaults()
	cfg.HTTP.Addr = "192.168.16.5:7777"
	cfg.HTTP.PublicOrigin = "https://heyarr.br.example.com"
	if got, want := rendererBaseURL(cfg), "https://heyarr.br.example.com"; got != want {
		t.Fatalf("without a render listener renderers get the public origin: got %q want %q", got, want)
	}
	cfg.HTTP.RenderAddr = "192.168.16.5:7778"
	if got, want := rendererBaseURL(cfg), "http://192.168.16.5:7778"; got != want {
		t.Fatalf("with a render listener renderers get it, plain: got %q want %q", got, want)
	}
	if got, want := renderBaseURL(cfg), "https://heyarr.br.example.com"; got != want {
		t.Fatalf("the login origin must not move to the plain listener: got %q want %q", got, want)
	}
}
