package renderroute

import (
	"testing"
)

// Tests cover every rule in Plan and the helpers linkKey, headroom, and
// buildAlternatives. Site names and IPs use the RFC 5737 reserved test
// spaces (192.0.2.x, 198.51.100.x) and generic names ("site-a", "site-b")
// so nothing here names a real host or network.

// makeLink returns a LinkCapacities map with one entry for the given pair.
func makeLink(a, b string, mbit float64) map[[2]string]float64 {
	return map[[2]string]float64{
		linkKey(a, b): mbit,
	}
}

func TestPlan_Rule1_R3OnlyRenderer(t *testing.T) {
	in := Input{
		RendererReachable: false,
		SourceFound:       true,
		Source: Source{
			PeerID:   "peer-a",
			Site:     "site-a",
			IsSelf:   true,
			Endpoint: "",
		},
		RendererSite:     "site-a",
		ControllerPeerID: "peer-a",
	}
	r := Plan(in)
	if !r.Refused {
		t.Error("want Refused=true for r3_only renderer, got false")
	}
	if r.ReasonCode != "route-r3-not-available" {
		t.Errorf("want ReasonCode=route-r3-not-available, got %q", r.ReasonCode)
	}
	if r.Route != "" {
		t.Errorf("want no Route for refusal, got %q", r.Route)
	}
}

func TestPlan_Rule2_NoSource(t *testing.T) {
	in := Input{
		RendererReachable: true,
		SourceFound:       false,
		RendererSite:      "site-a",
		ControllerPeerID:  "peer-a",
	}
	r := Plan(in)
	if !r.Refused {
		t.Error("want Refused=true for no source, got false")
	}
	if r.ReasonCode != "route-no-source" {
		t.Errorf("want ReasonCode=route-no-source, got %q", r.ReasonCode)
	}
}

func TestPlan_Rule3_R1_SameSite(t *testing.T) {
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID:   "peer-a",
			Site:     "site-a",
			IsSelf:   true,
			Endpoint: "",
		},
		RendererSite:     "site-a",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
	}
	r := Plan(in)
	if r.Refused {
		t.Errorf("want Refused=false for R1, got true (reason: %s)", r.ReasonCode)
	}
	if r.Route != "r1" {
		t.Errorf("want Route=r1, got %q", r.Route)
	}
	if r.ReasonCode != "route-r1-local" {
		t.Errorf("want ReasonCode=route-r1-local, got %q", r.ReasonCode)
	}
	if r.ByteOrigin != "http://192.0.2.10:7777" {
		t.Errorf("want ByteOrigin=http://192.0.2.10:7777, got %q", r.ByteOrigin)
	}
	if r.ControllingParty != "peer-a" {
		t.Errorf("want ControllingParty=peer-a, got %q", r.ControllingParty)
	}
}

func TestPlan_Rule3_R1_SameSite_RemoteSource(t *testing.T) {
	// Source is on same site as renderer but is NOT self: byteOrigin = source endpoint.
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID:   "peer-b",
			Site:     "site-a",
			IsSelf:   false,
			Endpoint: "http://198.51.100.20:7777",
		},
		RendererSite:     "site-a",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
	}
	r := Plan(in)
	if r.Refused {
		t.Errorf("want Refused=false for R1, got true (reason: %s)", r.ReasonCode)
	}
	if r.Route != "r1" {
		t.Errorf("want Route=r1, got %q", r.Route)
	}
	if r.ByteOrigin != "http://198.51.100.20:7777" {
		t.Errorf("want ByteOrigin=http://198.51.100.20:7777, got %q", r.ByteOrigin)
	}
}

func TestPlan_Rule4_R2_CapacityUndeclared(t *testing.T) {
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID:   "peer-a",
			Site:     "site-a",
			IsSelf:   true,
			Endpoint: "",
		},
		RendererSite:     "site-b",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
		// No LinkCapacities entry for site-a <-> site-b.
		LinkCapacities: map[[2]string]float64{},
	}
	r := Plan(in)
	if r.Refused {
		t.Errorf("want Refused=false for undeclared capacity, got true (reason: %s)", r.ReasonCode)
	}
	if r.Route != "r2" {
		t.Errorf("want Route=r2, got %q", r.Route)
	}
	if r.ReasonCode != "route-r2-capacity-undeclared" {
		t.Errorf("want ReasonCode=route-r2-capacity-undeclared, got %q", r.ReasonCode)
	}
}

func TestPlan_Rule4_R2_CapacityUndeclared_NilMap(t *testing.T) {
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID: "peer-a",
			Site:   "site-a",
			IsSelf: true,
		},
		RendererSite:     "site-b",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
		// nil map: no capacities declared at all.
	}
	r := Plan(in)
	if r.Refused {
		t.Errorf("want Refused=false for nil link capacities, got true (reason: %s)", r.ReasonCode)
	}
	if r.ReasonCode != "route-r2-capacity-undeclared" {
		t.Errorf("want ReasonCode=route-r2-capacity-undeclared, got %q", r.ReasonCode)
	}
}

func TestPlan_Rule4_R2_BitrateUnknown(t *testing.T) {
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID: "peer-a",
			Site:   "site-a",
			IsSelf: true,
		},
		RendererSite:     "site-b",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
		AssetBitrateKbps: 0, // unknown
		LinkCapacities:   makeLink("site-a", "site-b", 100.0),
	}
	r := Plan(in)
	if r.Refused {
		t.Errorf("want Refused=false for unknown bitrate, got true (reason: %s)", r.ReasonCode)
	}
	if r.Route != "r2" {
		t.Errorf("want Route=r2, got %q", r.Route)
	}
	if r.ReasonCode != "route-r2-bitrate-unknown" {
		t.Errorf("want ReasonCode=route-r2-bitrate-unknown, got %q", r.ReasonCode)
	}
}

func TestPlan_Rule4_R2_WithinCapacity(t *testing.T) {
	// 10 Mbit/s link, 5000 kbps asset, headroom 1.5 -> 7.5 Mbit/s needed <= 10.
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID: "peer-a",
			Site:   "site-a",
			IsSelf: true,
		},
		RendererSite:     "site-b",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
		AssetBitrateKbps: 5000, // 5 Mbit/s
		HeadroomFactor:   1.5,
		LinkCapacities:   makeLink("site-a", "site-b", 10.0),
	}
	r := Plan(in)
	if r.Refused {
		t.Errorf("want Refused=false for within-capacity R2, got true (reason: %s)", r.ReasonCode)
	}
	if r.Route != "r2" {
		t.Errorf("want Route=r2, got %q", r.Route)
	}
	if r.ReasonCode != "route-r2-within-capacity" {
		t.Errorf("want ReasonCode=route-r2-within-capacity, got %q", r.ReasonCode)
	}
}

func TestPlan_Rule4_R2_OverCapacity_NoAlternatives(t *testing.T) {
	// 5 Mbit/s link, 5000 kbps asset, headroom 1.5 -> 7.5 Mbit/s needed > 5.
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID: "peer-a",
			Site:   "site-a",
			IsSelf: true,
		},
		RendererSite:        "site-b",
		RenderBaseURL:       "http://192.0.2.10:7777",
		ControllerPeerID:    "peer-a",
		AssetBitrateKbps:    5000, // 5 Mbit/s
		HeadroomFactor:      1.5,
		LinkCapacities:      makeLink("site-a", "site-b", 5.0),
		RendererSiteHasPeer: false,
	}
	r := Plan(in)
	if !r.Refused {
		t.Error("want Refused=true for over-capacity R2, got false")
	}
	if r.ReasonCode != "route-r2-over-capacity" {
		t.Errorf("want ReasonCode=route-r2-over-capacity, got %q", r.ReasonCode)
	}
	if len(r.Alternatives) != 0 {
		t.Errorf("want no alternatives when no siblings and no peer, got %d", len(r.Alternatives))
	}
}

func TestPlan_Rule4_R2_OverCapacity_WithSmallerSiblings(t *testing.T) {
	// 5 Mbit/s link, 5000 kbps asset too big. Two siblings: 2000 kbps fits, 4500 kbps does not.
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID: "peer-a",
			Site:   "site-a",
			IsSelf: true,
		},
		RendererSite:     "site-b",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
		AssetBitrateKbps: 5000,
		HeadroomFactor:   1.5,
		LinkCapacities:   makeLink("site-a", "site-b", 5.0),
		SiblingAssets: []SiblingAsset{
			{AssetID: "asset-720p", HeightPx: 720, BitrateKbps: 2000},   // 3 Mbit/s <= 5: fits
			{AssetID: "asset-1080p", HeightPx: 1080, BitrateKbps: 4500}, // 6.75 Mbit/s > 5: does not fit
		},
	}
	r := Plan(in)
	if !r.Refused {
		t.Error("want Refused=true for over-capacity R2")
	}
	if len(r.Alternatives) != 1 {
		t.Fatalf("want 1 alternative (720p fits), got %d", len(r.Alternatives))
	}
	alt := r.Alternatives[0]
	if alt.Kind != "smaller_copy" {
		t.Errorf("want Kind=smaller_copy, got %q", alt.Kind)
	}
	if alt.AssetID != "asset-720p" {
		t.Errorf("want AssetID=asset-720p, got %q", alt.AssetID)
	}
	if alt.HeightPx != 720 {
		t.Errorf("want HeightPx=720, got %d", alt.HeightPx)
	}
}

func TestPlan_Rule4_R2_OverCapacity_AlternativesSortedByHeight(t *testing.T) {
	// Both siblings fit; should be sorted height-descending.
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID: "peer-a",
			Site:   "site-a",
			IsSelf: true,
		},
		RendererSite:     "site-b",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
		AssetBitrateKbps: 20000,
		HeadroomFactor:   1.5,
		LinkCapacities:   makeLink("site-a", "site-b", 5.0),
		SiblingAssets: []SiblingAsset{
			{AssetID: "asset-480p", HeightPx: 480, BitrateKbps: 1000},
			{AssetID: "asset-720p", HeightPx: 720, BitrateKbps: 2000},
		},
	}
	r := Plan(in)
	if !r.Refused {
		t.Error("want Refused=true for over-capacity R2")
	}
	if len(r.Alternatives) != 2 {
		t.Fatalf("want 2 alternatives, got %d", len(r.Alternatives))
	}
	if r.Alternatives[0].HeightPx != 720 {
		t.Errorf("want first alternative HeightPx=720 (highest quality first), got %d", r.Alternatives[0].HeightPx)
	}
	if r.Alternatives[1].HeightPx != 480 {
		t.Errorf("want second alternative HeightPx=480, got %d", r.Alternatives[1].HeightPx)
	}
}

func TestPlan_Rule4_R2_OverCapacity_CopyToSiteFirst(t *testing.T) {
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID: "peer-a",
			Site:   "site-a",
			IsSelf: true,
		},
		RendererSite:        "site-b",
		RenderBaseURL:       "http://192.0.2.10:7777",
		ControllerPeerID:    "peer-a",
		AssetBitrateKbps:    5000,
		HeadroomFactor:      1.5,
		LinkCapacities:      makeLink("site-a", "site-b", 5.0),
		RendererSiteHasPeer: true,
	}
	r := Plan(in)
	if !r.Refused {
		t.Error("want Refused=true for over-capacity R2")
	}
	found := false
	for _, alt := range r.Alternatives {
		if alt.Kind == "copy_to_site_first" {
			found = true
			if alt.Site != "site-b" {
				t.Errorf("want copy_to_site_first Site=site-b, got %q", alt.Site)
			}
		}
	}
	if !found {
		t.Error("want a copy_to_site_first alternative when renderer site has a peer")
	}
}

func TestPlan_Rule4_R2_OverCapacity_BothAlternatives(t *testing.T) {
	// Fits sibling + copy_to_site_first should both appear when conditions hold.
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID: "peer-a",
			Site:   "site-a",
			IsSelf: true,
		},
		RendererSite:        "site-b",
		RenderBaseURL:       "http://192.0.2.10:7777",
		ControllerPeerID:    "peer-a",
		AssetBitrateKbps:    5000,
		HeadroomFactor:      1.5,
		LinkCapacities:      makeLink("site-a", "site-b", 5.0),
		RendererSiteHasPeer: true,
		SiblingAssets: []SiblingAsset{
			{AssetID: "asset-480p", HeightPx: 480, BitrateKbps: 1500},
		},
	}
	r := Plan(in)
	if !r.Refused {
		t.Error("want Refused=true for over-capacity R2")
	}
	smallerCount, copyCount := 0, 0
	for _, alt := range r.Alternatives {
		switch alt.Kind {
		case "smaller_copy":
			smallerCount++
		case "copy_to_site_first":
			copyCount++
		}
	}
	if smallerCount != 1 {
		t.Errorf("want 1 smaller_copy alternative, got %d", smallerCount)
	}
	if copyCount != 1 {
		t.Errorf("want 1 copy_to_site_first alternative, got %d", copyCount)
	}
}

func TestPlan_Rule4_R2_ZeroBitrateSibling_Skipped(t *testing.T) {
	// Sibling with zero bitrate must not appear in alternatives.
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID: "peer-a",
			Site:   "site-a",
			IsSelf: true,
		},
		RendererSite:     "site-b",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
		AssetBitrateKbps: 5000,
		HeadroomFactor:   1.5,
		LinkCapacities:   makeLink("site-a", "site-b", 5.0),
		SiblingAssets: []SiblingAsset{
			{AssetID: "asset-720p", HeightPx: 720, BitrateKbps: 0}, // zero = unknown
		},
	}
	r := Plan(in)
	if !r.Refused {
		t.Error("want Refused=true")
	}
	for _, alt := range r.Alternatives {
		if alt.Kind == "smaller_copy" && alt.AssetID == "asset-720p" {
			t.Error("sibling with unknown bitrate must not appear in alternatives")
		}
	}
}

func TestPlan_DefaultHeadroom(t *testing.T) {
	// Zero HeadroomFactor must default to 1.5 -> 5000 * 1.5/1000 = 7.5 Mbit/s.
	// A 10 Mbit/s link should admit it.
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID: "peer-a",
			Site:   "site-a",
			IsSelf: true,
		},
		RendererSite:     "site-b",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
		AssetBitrateKbps: 5000,
		HeadroomFactor:   0, // must default to 1.5
		LinkCapacities:   makeLink("site-a", "site-b", 10.0),
	}
	r := Plan(in)
	if r.Refused {
		t.Errorf("want Refused=false with default headroom 1.5, got true (reason: %s)", r.ReasonCode)
	}
}

func TestPlan_LinkKeySymmetry(t *testing.T) {
	// The capacity map uses sorted keys. site-b->site-a lookup must find the entry
	// stored under site-a->site-b.
	caps := makeLink("site-a", "site-b", 10.0)
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID:   "peer-b",
			Site:     "site-b",
			IsSelf:   false,
			Endpoint: "http://198.51.100.20:7777",
		},
		RendererSite:     "site-a",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
		AssetBitrateKbps: 2000, // 3 Mbit/s with headroom <= 10
		HeadroomFactor:   1.5,
		LinkCapacities:   caps,
	}
	r := Plan(in)
	if r.Refused {
		t.Errorf("want Refused=false (reversed site lookup), got true (reason: %s)", r.ReasonCode)
	}
	if r.ReasonCode != "route-r2-within-capacity" {
		t.Errorf("want route-r2-within-capacity, got %q", r.ReasonCode)
	}
}

func TestPlan_ByteOrigin_SelfSource(t *testing.T) {
	// When source.IsSelf=true and endpoint is empty, ByteOrigin = RenderBaseURL.
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID:   "peer-a",
			Site:     "site-a",
			IsSelf:   true,
			Endpoint: "",
		},
		RendererSite:     "site-a",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
	}
	r := Plan(in)
	if r.ByteOrigin != "http://192.0.2.10:7777" {
		t.Errorf("want ByteOrigin=http://192.0.2.10:7777, got %q", r.ByteOrigin)
	}
}

func TestPlan_ByteOrigin_RemoteSource(t *testing.T) {
	// When source.IsSelf=false, ByteOrigin = source.Endpoint.
	in := Input{
		RendererReachable: true,
		SourceFound:       true,
		Source: Source{
			PeerID:   "peer-b",
			Site:     "site-b",
			IsSelf:   false,
			Endpoint: "http://198.51.100.20:7777",
		},
		RendererSite:     "site-b",
		RenderBaseURL:    "http://192.0.2.10:7777",
		ControllerPeerID: "peer-a",
	}
	r := Plan(in)
	if r.ByteOrigin != "http://198.51.100.20:7777" {
		t.Errorf("want ByteOrigin=http://198.51.100.20:7777, got %q", r.ByteOrigin)
	}
}

func TestPlan_Rule1_PrecedesRule3(t *testing.T) {
	// A renderer that is r3_only AND on the same site as the source must still be refused.
	in := Input{
		RendererReachable: false, // r3_only
		SourceFound:       true,
		Source: Source{
			PeerID: "peer-a",
			Site:   "site-a",
			IsSelf: true,
		},
		RendererSite:     "site-a",
		ControllerPeerID: "peer-a",
	}
	r := Plan(in)
	if !r.Refused {
		t.Error("want Refused=true: rule 1 (r3_only) beats rule 3 (same site)")
	}
	if r.ReasonCode != "route-r3-not-available" {
		t.Errorf("want ReasonCode=route-r3-not-available, got %q", r.ReasonCode)
	}
}

func TestPlan_Rule2_PrecedesRule4(t *testing.T) {
	// No source takes precedence over cross-site capacity logic.
	in := Input{
		RendererReachable: true,
		SourceFound:       false,
		RendererSite:      "site-b",
		LinkCapacities:    makeLink("site-a", "site-b", 100.0),
		ControllerPeerID:  "peer-a",
	}
	r := Plan(in)
	if !r.Refused {
		t.Error("want Refused=true: no source")
	}
	if r.ReasonCode != "route-no-source" {
		t.Errorf("want ReasonCode=route-no-source, got %q", r.ReasonCode)
	}
}
