package resources_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/config"
)

// minimalUPnPDescription is a minimal valid UPnP MediaRenderer device description.
// Uses a loopback-local httptest.Server; the SSRF guard permits loopback in single-site mode.
const minimalUPnPDescription = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType>
    <friendlyName>Test Renderer</friendlyName>
    <manufacturer>Test Vendor</manufacturer>
    <modelName>Test Model</modelName>
    <UDN>uuid:test-renderer-0001</UDN>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
        <controlURL>/upnp/control/AVTransport</controlURL>
      </service>
    </serviceList>
  </device>
</root>`

// loopbackSiteMap returns a SiteMap with a single site covering 127.0.0.0/8,
// which includes the address httptest.Server listens on.
func loopbackSiteMap(t *testing.T) config.SiteMap {
	t.Helper()
	sm, err := config.ParseSites(map[string][]string{
		"site-local": {"127.0.0.0/8"},
	}, "site-local")
	if err != nil {
		t.Fatal(err)
	}
	return sm
}

// twoSiteMap returns a SiteMap with site-local (loopback) and site-remote
// (TEST-NET-3, 203.0.113.0/24) as two distinct sites.
func twoSiteMap(t *testing.T) config.SiteMap {
	t.Helper()
	sm, err := config.ParseSites(map[string][]string{
		"site-local":  {"127.0.0.0/8"},
		"site-remote": {"203.0.113.0/24"},
	}, "site-local")
	if err != nil {
		t.Fatal(err)
	}
	return sm
}

// startDescriptionServer starts an httptest.Server that always returns desc.
func startDescriptionServer(t *testing.T, desc string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		_, _ = w.Write([]byte(desc))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// reportBody builds the JSON body for POST /api/v1/renderers/reported.
func reportBody(location string) *strings.Reader {
	return strings.NewReader(`{"location":"` + location + `"}`)
}

// TestReportAndListRenderer verifies the happy path: POST reports a renderer,
// then GET lists it with the expected fields.
func TestReportAndListRenderer(t *testing.T) {
	descSrv := startDescriptionServer(t, minimalUPnPDescription)
	h := newHarness(t, withAuth, withSites(loopbackSiteMap(t)))
	tok := h.mint("alice", auth.ScopeRead, auth.ScopeWrite)

	// Report the renderer.
	resp := h.do(http.MethodPost, "/api/v1/renderers/reported", tok.Secret, reportBody(descSrv.URL+"/desc"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST reported: want 200, got %d; body: %s", resp.StatusCode, h.body(resp))
	}

	var posted resources.RendererView
	if err := json.Unmarshal(h.body(resp), &posted); err != nil {
		t.Fatal(err)
	}
	if posted.UDN == "" {
		t.Error("response UDN is empty")
	}
	if posted.DiscoveredBy != "client" {
		t.Errorf("DiscoveredBy = %q, want %q", posted.DiscoveredBy, "client")
	}
	if posted.Site == "" {
		t.Error("response Site is empty")
	}

	// List renderers.
	listResp := h.do(http.MethodGet, "/api/v1/renderers", tok.Secret, nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET renderers: want 200, got %d; body: %s", listResp.StatusCode, h.body(listResp))
	}

	var list struct {
		ClientSite string                   `json:"client_site"`
		Renderers  []resources.RendererView `json:"renderers"`
	}
	if err := json.Unmarshal(h.body(listResp), &list); err != nil {
		t.Fatal(err)
	}
	if list.ClientSite == "" {
		t.Error("client_site is empty")
	}
	found := false
	for _, rv := range list.Renderers {
		if rv.UDN == posted.UDN {
			found = true
			if rv.DiscoveredBy != "client" {
				t.Errorf("listed DiscoveredBy = %q, want %q", rv.DiscoveredBy, "client")
			}
			if rv.Site == "" {
				t.Error("listed Site is empty")
			}
		}
	}
	if !found {
		t.Errorf("reported renderer %q not found in list", posted.UDN)
	}
}

// TestClientRendererTTLExpiry reports a renderer, advances the clock past TTL,
// and verifies it no longer appears in the list.
func TestClientRendererTTLExpiry(t *testing.T) {
	descSrv := startDescriptionServer(t, minimalUPnPDescription)
	h := newHarness(t, withAuth, withSites(loopbackSiteMap(t)))
	tok := h.mint("alice", auth.ScopeRead, auth.ScopeWrite)

	resp := h.do(http.MethodPost, "/api/v1/renderers/reported", tok.Secret, reportBody(descSrv.URL+"/desc"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST reported: want 200, got %d; body: %s", resp.StatusCode, h.body(resp))
	}

	// Advance clock past TTL (clientRendererTTL = 10 minutes).
	h.clock.t = h.clock.t.Add(10*time.Minute + time.Second)

	listResp := h.do(http.MethodGet, "/api/v1/renderers", tok.Secret, nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET renderers: want 200, got %d; body: %s", listResp.StatusCode, h.body(listResp))
	}
	var list struct {
		Renderers []resources.RendererView `json:"renderers"`
	}
	if err := json.Unmarshal(h.body(listResp), &list); err != nil {
		t.Fatal(err)
	}
	// Controller-discovered renderers will also be in the list; we specifically
	// care that no renderer with DiscoveredBy="client" is present after expiry.
	for _, rv := range list.Renderers {
		if rv.DiscoveredBy == "client" {
			t.Errorf("expired client renderer %q still in list", rv.UDN)
		}
	}
}

// TestClientSiteInResponse verifies that GET /renderers includes client_site
// derived from the request's remote address, and that a reported renderer
// carries the correct site field matching where its description server lives.
func TestClientSiteInResponse(t *testing.T) {
	sm := twoSiteMap(t)
	descSrv := startDescriptionServer(t, minimalUPnPDescription)
	h := newHarness(t, withAuth, withSites(sm))
	tok := h.mint("alice", auth.ScopeRead, auth.ScopeWrite)

	// Report the renderer. The httptest.Server is on loopback (127.0.0.1),
	// which falls inside site-local per twoSiteMap.
	resp := h.do(http.MethodPost, "/api/v1/renderers/reported", tok.Secret, reportBody(descSrv.URL+"/desc"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST reported: want 200, got %d; body: %s", resp.StatusCode, h.body(resp))
	}
	var posted resources.RendererView
	if err := json.Unmarshal(h.body(resp), &posted); err != nil {
		t.Fatal(err)
	}
	if posted.Site != "site-local" {
		t.Errorf("POST response site = %q, want site-local", posted.Site)
	}

	// GET renderers.
	listResp := h.do(http.MethodGet, "/api/v1/renderers", tok.Secret, nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET renderers: want 200, got %d; body: %s", listResp.StatusCode, h.body(listResp))
	}
	var list struct {
		ClientSite string                   `json:"client_site"`
		Renderers  []resources.RendererView `json:"renderers"`
	}
	if err := json.Unmarshal(h.body(listResp), &list); err != nil {
		t.Fatal(err)
	}

	// harness requests originate from 127.0.0.1 → site-local.
	if list.ClientSite != "site-local" {
		t.Errorf("client_site = %q, want site-local", list.ClientSite)
	}

	// The reported renderer (on loopback) should carry site "site-local".
	found := false
	for _, rv := range list.Renderers {
		if rv.UDN == posted.UDN {
			found = true
			if rv.Site != "site-local" {
				t.Errorf("listed renderer site = %q, want site-local", rv.Site)
			}
		}
	}
	if !found {
		t.Fatal("reported renderer not found in list")
	}
}

// TestR3OnlyVisibility verifies that an r3_only entry is visible to the
// reporting principal but NOT to a different principal.
func TestR3OnlyVisibility(t *testing.T) {
	// Use single-site mode; 203.0.113.1 is public → r3_only.
	h := newHarness(t, withAuth)
	aliceTok := h.mint("alice", auth.ScopeRead, auth.ScopeWrite)
	bobTok := h.mint("bob", auth.ScopeRead, auth.ScopeWrite)

	// Report a URL with a public IP — should be r3_only.
	r3URL := "http://203.0.113.1/desc"
	resp := h.do(http.MethodPost, "/api/v1/renderers/reported", aliceTok.Secret, reportBody(r3URL))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST reported: want 202, got %d; body: %s", resp.StatusCode, h.body(resp))
	}
	var r3resp struct {
		R3Only   bool   `json:"r3_only"`
		Location string `json:"location"`
	}
	if err := json.Unmarshal(h.body(resp), &r3resp); err != nil {
		t.Fatal(err)
	}
	if !r3resp.R3Only {
		t.Error("r3_only response field should be true")
	}
	if r3resp.Location != r3URL {
		t.Errorf("location = %q, want %q", r3resp.Location, r3URL)
	}

	// Alice lists renderers — should see the r3_only entry.
	aliceList := h.do(http.MethodGet, "/api/v1/renderers", aliceTok.Secret, nil)
	if aliceList.StatusCode != http.StatusOK {
		t.Fatalf("GET renderers alice: want 200, got %d", aliceList.StatusCode)
	}
	var alice struct {
		Renderers []resources.RendererView `json:"renderers"`
	}
	if err := json.Unmarshal(h.body(aliceList), &alice); err != nil {
		t.Fatal(err)
	}
	aliceFound := false
	for _, rv := range alice.Renderers {
		if rv.Location == r3URL && rv.R3Only {
			aliceFound = true
		}
	}
	if !aliceFound {
		t.Error("alice should see her own r3_only entry in the renderer list")
	}

	// Bob lists renderers — should NOT see Alice's r3_only entry.
	bobList := h.do(http.MethodGet, "/api/v1/renderers", bobTok.Secret, nil)
	if bobList.StatusCode != http.StatusOK {
		t.Fatalf("GET renderers bob: want 200, got %d", bobList.StatusCode)
	}
	var bob struct {
		Renderers []resources.RendererView `json:"renderers"`
	}
	if err := json.Unmarshal(h.body(bobList), &bob); err != nil {
		t.Fatal(err)
	}
	for _, rv := range bob.Renderers {
		if rv.Location == r3URL {
			t.Errorf("bob should not see alice's r3_only entry, but found %+v", rv)
		}
	}
}

// TestSSRFRedirectRefused verifies that the fetch client used by reportRenderer
// does not follow redirects: a description server that always 302-redirects
// should cause a 502 rather than following to the redirect target.
func TestSSRFRedirectRefused(t *testing.T) {
	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1/malicious", http.StatusFound)
	}))
	t.Cleanup(redirectSrv.Close)

	h := newHarness(t, withAuth, withSites(loopbackSiteMap(t)))
	tok := h.mint("alice", auth.ScopeRead, auth.ScopeWrite)

	resp := h.do(http.MethodPost, "/api/v1/renderers/reported", tok.Secret, reportBody(redirectSrv.URL+"/desc"))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("redirect should yield 502, got %d; body: %s", resp.StatusCode, h.body(resp))
	}
}

// TestSSRFSchemeRejected verifies that non-http schemes are rejected with 400.
func TestSSRFSchemeRejected(t *testing.T) {
	h := newHarness(t, withAuth, withSites(loopbackSiteMap(t)))
	tok := h.mint("alice", auth.ScopeRead, auth.ScopeWrite)

	resp := h.do(http.MethodPost, "/api/v1/renderers/reported", tok.Secret, reportBody("https://127.0.0.1/desc"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("https URL should yield 400, got %d; body: %s", resp.StatusCode, h.body(resp))
	}
}

// TestSSRFHostnameRejected verifies that hostname (not literal IP) URLs are rejected with 400.
func TestSSRFHostnameRejected(t *testing.T) {
	h := newHarness(t, withAuth, withSites(loopbackSiteMap(t)))
	tok := h.mint("alice", auth.ScopeRead, auth.ScopeWrite)

	resp := h.do(http.MethodPost, "/api/v1/renderers/reported", tok.Secret, reportBody("http://my-renderer.local/desc"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("hostname URL should yield 400, got %d; body: %s", resp.StatusCode, h.body(resp))
	}
}

// TestReportRendererMissingLocation verifies that a missing location field returns 400.
func TestReportRendererMissingLocation(t *testing.T) {
	h := newHarness(t, withAuth, withSites(loopbackSiteMap(t)))
	tok := h.mint("alice", auth.ScopeRead, auth.ScopeWrite)

	resp := h.do(http.MethodPost, "/api/v1/renderers/reported", tok.Secret, strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing location should yield 400, got %d; body: %s", resp.StatusCode, h.body(resp))
	}
}
