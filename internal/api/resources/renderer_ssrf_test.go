package resources

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/renderer"
)

// singleSite is the zero SiteMap (single-site mode, no configured CIDRs).
var singleSiteMap = config.SiteMap{}

// multiSiteMap has one site covering TEST-NET-1 (192.0.2.0/24, RFC 5737).
func multiSiteMap(t *testing.T) config.SiteMap {
	t.Helper()
	sm, err := config.ParseSites(map[string][]string{
		"site-a": {"192.0.2.0/24"},
	}, "site-a")
	if err != nil {
		t.Fatal(err)
	}
	return sm
}

func TestCheckRendererURL(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		sites      func(*testing.T) config.SiteMap
		wantResult ssrfResult
		wantCode   string
	}{
		// --- scheme rejections ---
		{
			name:       "https rejected",
			url:        "https://192.0.2.1/desc",
			sites:      func(t *testing.T) config.SiteMap { return singleSiteMap },
			wantResult: ssrfRejected,
			wantCode:   ssrfCodeBadScheme,
		},
		{
			name:       "ftp rejected",
			url:        "ftp://192.0.2.1/desc",
			sites:      func(t *testing.T) config.SiteMap { return singleSiteMap },
			wantResult: ssrfRejected,
			wantCode:   ssrfCodeBadScheme,
		},
		{
			name:       "unparseable URL rejected",
			url:        "://bad",
			sites:      func(t *testing.T) config.SiteMap { return singleSiteMap },
			wantResult: ssrfRejected,
			wantCode:   ssrfCodeBadScheme,
		},
		// --- hostname rejection ---
		{
			name:       "hostname not allowed",
			url:        "http://hostname.example/desc",
			sites:      func(t *testing.T) config.SiteMap { return singleSiteMap },
			wantResult: ssrfRejected,
			wantCode:   ssrfCodeHostname,
		},
		// --- userinfo rejection ---
		{
			name:       "userinfo not allowed",
			url:        "http://user:pass@192.0.2.1/desc",
			sites:      func(t *testing.T) config.SiteMap { return singleSiteMap },
			wantResult: ssrfRejected,
			wantCode:   ssrfCodeUserinfo,
		},
		// --- zero port rejection ---
		{
			name:       "zero port rejected",
			url:        "http://192.0.2.1:0/desc",
			sites:      func(t *testing.T) config.SiteMap { return singleSiteMap },
			wantResult: ssrfRejected,
			wantCode:   ssrfCodeZeroPort,
		},
		// --- single-site r3_only (public IP) ---
		{
			name:       "single-site public IP is r3_only",
			url:        "http://192.0.2.1/desc",
			sites:      func(t *testing.T) config.SiteMap { return singleSiteMap },
			wantResult: ssrfR3Only,
			wantCode:   ssrfCodeOutsideSites,
		},
		// --- single-site OK (non-public) ---
		{
			name:       "loopback refused",
			url:        "http://127.0.0.1/desc",
			sites:      func(t *testing.T) config.SiteMap { return singleSiteMap },
			wantResult: ssrfRejected,
			wantCode:   ssrfCodeSpecialAddr,
		},
		{
			name:       "IPv6 link-local refused",
			url:        "http://[fe80::1]/desc",
			sites:      func(t *testing.T) config.SiteMap { return singleSiteMap },
			wantResult: ssrfRejected,
			wantCode:   ssrfCodeSpecialAddr,
		},
		{
			name:       "cloud metadata address refused",
			url:        "http://169.254.169.254/latest/meta-data/",
			sites:      func(t *testing.T) config.SiteMap { return singleSiteMap },
			wantResult: ssrfRejected,
			wantCode:   ssrfCodeSpecialAddr,
		},
		{
			name: "loopback refused even when a site covers it",
			url:  "http://127.0.0.1:7777/",
			sites: func(t *testing.T) config.SiteMap {
				sm, err := config.ParseSites(map[string][]string{"lo": {"127.0.0.0/8"}}, "lo")
				if err != nil {
					t.Fatal(err)
				}
				return sm
			},
			wantResult: ssrfRejected,
			wantCode:   ssrfCodeSpecialAddr,
		},
		{
			name:       "multicast refused",
			url:        "http://239.255.255.250:1900/",
			sites:      func(t *testing.T) config.SiteMap { return singleSiteMap },
			wantResult: ssrfRejected,
			wantCode:   ssrfCodeSpecialAddr,
		},
		{
			name:       "single-site private is OK",
			url:        "http://10.0.0.1/desc",
			sites:      func(t *testing.T) config.SiteMap { return singleSiteMap },
			wantResult: ssrfOK,
		},
		// --- multi-site OK ---
		{
			name:       "multi-site IP inside site is OK",
			url:        "http://192.0.2.1/desc",
			sites:      multiSiteMap,
			wantResult: ssrfOK,
		},
		// --- multi-site r3_only (IP outside configured sites) ---
		{
			name:       "multi-site IP outside all sites is r3_only",
			url:        "http://203.0.113.1/desc", // TEST-NET-3, not in site-a
			sites:      multiSiteMap,
			wantResult: ssrfR3Only,
			wantCode:   ssrfCodeOutsideSites,
		},
		{
			name:       "multi-site private IP outside all sites is r3_only",
			url:        "http://10.0.0.1/desc", // private, not in site-a's 192.0.2.0/24
			sites:      multiSiteMap,
			wantResult: ssrfR3Only,
			wantCode:   ssrfCodeOutsideSites,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm := tc.sites(t)
			result, rerr := checkRendererURL(tc.url, sm, false)
			if result != tc.wantResult {
				t.Errorf("result = %v, want %v", result, tc.wantResult)
			}
			if tc.wantCode != "" {
				if rerr == nil {
					t.Fatal("expected non-nil error, got nil")
				}
				if rerr.code != tc.wantCode {
					t.Errorf("code = %q, want %q", rerr.code, tc.wantCode)
				}
			}
			if tc.wantResult == ssrfOK && rerr != nil {
				t.Errorf("expected nil error for ssrfOK, got %v", rerr)
			}
		})
	}
}

// TestCheckRendererURLRedirectNotFollowed verifies that the SSRF guard itself
// does not follow redirects — the guard is a pure URL check, so it does not
// fetch anything. This test uses an httptest.Server that always 302-redirects
// to ensure that even if the guard were to fetch (it shouldn't), the redirect
// target does not reach our httptest.Server.
//
// The actual redirect-not-followed behaviour is exercised in the integration
// test for the reportRenderer handler, where the fetch client is created.
func TestCheckRendererURLRedirectNotFollowed(t *testing.T) {
	// Set up a server that always redirects.
	redirected := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		http.Redirect(w, r, "http://192.0.2.99/other", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	// The server is on 127.0.0.1 (loopback); the test seam admits it.
	// The guard is pure; it must not fetch anything.
	result, rerr := checkRendererURL(srv.URL+"/desc", singleSiteMap, true)
	if result != ssrfOK {
		t.Errorf("result = %v, want ssrfOK; rerr = %v", result, rerr)
	}
	if redirected {
		t.Error("checkRendererURL should not have made an HTTP request")
	}
}

func TestCheckReportedControlURLs(t *testing.T) {
	const loc = "http://192.0.2.10:9197/dmr"
	cases := []struct {
		name    string
		control string
		wantErr bool
	}{
		{"same host and port", "http://192.0.2.10:9197/upnp/control/AVTransport1", false},
		{"other host", "http://192.0.2.11:9197/upnp/control/AVTransport1", true},
		{"loopback", "http://127.0.0.1:7777/api/v1/admin", true},
		{"same host other port", "http://192.0.2.10:80/ctl", true},
		{"https", "https://192.0.2.10:9197/ctl", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rend := renderer.Renderer{AVTransport: renderer.Service{Type: "urn:schemas-upnp-org:service:AVTransport:1", ControlURL: tc.control}}
			err := checkReportedControlURLs(loc, rend)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && err.code != ssrfCodeControlHost {
				t.Errorf("code = %q, want %q", err.code, ssrfCodeControlHost)
			}
		})
	}
}
