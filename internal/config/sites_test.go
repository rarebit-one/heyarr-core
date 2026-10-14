package config

import (
	"net/netip"
	"strings"
	"testing"
)

// TestSiteOfSingleSite checks single-site (no entries) mode:
// non-public → peerSite, public → "away".
func TestSiteOfSingleSite(t *testing.T) {
	sm, err := ParseSites(nil, "home")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		addr string
		want string
	}{
		{"10.0.0.1", "home"},       // private
		{"172.16.5.1", "home"},     // private
		{"192.168.1.1", "home"},    // private
		{"127.0.0.1", "home"},      // loopback
		{"::1", "home"},            // IPv6 loopback
		{"169.254.1.1", "home"},    // link-local unicast
		{"203.0.113.1", "away"},    // documentation range (public)
		{"198.51.100.1", "away"},   // documentation range (public)
		{"8.8.8.8", "away"},        // public
	}
	for _, tc := range cases {
		addr, err := netip.ParseAddr(tc.addr)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", tc.addr, err)
		}
		got := sm.SiteOf(addr)
		if got != tc.want {
			t.Errorf("SiteOf(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// TestSiteOfMultiSite checks multi-site (entries set) mode:
// IP in site-a → "site-a", in site-b → "site-b", elsewhere → "away".
func TestSiteOfMultiSite(t *testing.T) {
	sm, err := ParseSites(map[string][]string{
		"site-a": {"192.0.2.0/24"},    // TEST-NET-1 documentation range
		"site-b": {"198.51.100.0/24"}, // TEST-NET-2 documentation range
	}, "site-a")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		addr string
		want string
	}{
		{"192.0.2.1", "site-a"},
		{"192.0.2.254", "site-a"},
		{"198.51.100.1", "site-b"},
		{"10.0.0.1", "away"},         // private but not in any configured site
		{"127.0.0.1", "away"},        // loopback but not in any configured site
		{"203.0.113.1", "away"},      // TEST-NET-3 not in any site
		{"8.8.8.8", "away"},          // public not in any site
	}
	for _, tc := range cases {
		addr, err := netip.ParseAddr(tc.addr)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", tc.addr, err)
		}
		got := sm.SiteOf(addr)
		if got != tc.want {
			t.Errorf("SiteOf(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// TestParseSitesRejectsDuplicateCIDR ensures the same CIDR in two sites fails.
func TestParseSitesRejectsDuplicateCIDR(t *testing.T) {
	_, err := ParseSites(map[string][]string{
		"site-a": {"192.0.2.0/24"},
		"site-b": {"192.0.2.0/24"}, // exact same CIDR
	}, "site-a")
	if err == nil {
		t.Fatal("expected error for duplicate CIDR, got nil")
	}
}

// TestParseSitesRejectsOverlappingCIDR ensures overlapping (but not identical) CIDRs fail.
func TestParseSitesRejectsOverlappingCIDR(t *testing.T) {
	_, err := ParseSites(map[string][]string{
		"site-a": {"192.0.2.0/24"},
		"site-b": {"192.0.2.0/25"}, // /25 overlaps with /24
	}, "site-a")
	if err == nil {
		t.Fatal("expected error for overlapping CIDR, got nil")
	}
	if !strings.Contains(err.Error(), "overlap") {
		t.Errorf("error %q should mention overlap", err.Error())
	}
}

// TestParseSitesRejectsBadCIDR ensures an invalid CIDR string fails.
func TestParseSitesRejectsBadCIDR(t *testing.T) {
	_, err := ParseSites(map[string][]string{
		"site-a": {"not-a-cidr"},
	}, "site-a")
	if err == nil {
		t.Fatal("expected error for bad CIDR, got nil")
	}
}

// TestParseSitesRejectsEmptyName ensures a site with an empty name fails.
func TestParseSitesRejectsEmptyName(t *testing.T) {
	_, err := ParseSites(map[string][]string{
		"": {"192.0.2.0/24"},
	}, "site-a")
	if err == nil {
		t.Fatal("expected error for empty site name, got nil")
	}
}

// TestSsdpSearchAddrValidation checks config.Validate rejects wildcard / zero-port
// ssdp_search_addr values and accepts a concrete host:port.
func TestSsdpSearchAddrValidation(t *testing.T) {
	cases := []struct {
		addr    string
		wantErr bool
	}{
		{"0.0.0.0:1900", true},    // wildcard host
		{":1900", true},           // empty host
		{"192.0.2.1:1900", false}, // concrete, valid
		{"192.0.2.1:0", true},     // zero port
	}
	for _, tc := range cases {
		cfg := Defaults()
		cfg.Renderers.SsdpSearchAddr = tc.addr
		err := cfg.Validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("ssdp_search_addr=%q: Validate() error = %v, wantErr %v", tc.addr, err, tc.wantErr)
		}
	}
}
