// Package renderroute decides how bytes travel from their source to a renderer
// (ADR-0111 §R1/R2/R3).
//
// It is a pure function: no database, no clock, no network. The caller
// assembles an Input from the peers table, the routing decision, and the
// config; Plan returns a Result that carries either a route or a refusal with
// reasons.
//
// # Three routes, in order of preference
//
// R1 (local): the byte source is on the same site as the renderer. No
// inter-site link is crossed; capacity checking is skipped.
//
// R2 (cross-site): source and renderer are on different sites. The link
// capacity and the asset's bitrate determine whether the link can carry the
// stream; if it cannot the call is refused, never silently downgraded.
//
// R3 (relay): the renderer cannot be reached directly by any controller. Phase
// 1 does not implement R3; a renderer that requires relay is refused with
// route-r3-not-available.
//
// # Refusals carry alternatives
//
// An R2 refusal that detects a capacity problem lists smaller sibling copies
// that would fit the link, and (when the renderer's site has an enrolled peer)
// a copy-to-site-first option. The caller surfaces these to the client so the
// operator knows what to do next without a second round-trip.
package renderroute

import "sort"

// Input is everything Plan needs. Callers set only the fields they know; zero
// or false for unknown booleans means "absent", and zero for numeric fields
// means "unknown".
type Input struct {
	// RendererSite is the site name the renderer is in.
	RendererSite string
	// ClientSite is the site the requesting client is in. Used in future phases.
	ClientSite string
	// RendererReachable is false when the renderer is r3_only (relay only).
	// An unreachable renderer causes an immediate refusal.
	RendererReachable bool
	// Source is the routing.Candidate chosen by the §32 router. Zero value means no source.
	Source Source
	// SourceFound is true when Source is populated.
	SourceFound bool
	// AssetBitrateKbps is the asset's streaming bitrate in kbit/s. 0 = unknown.
	AssetBitrateKbps float64
	// LinkCapacities maps unordered site-pair keys to link capacity in Mbit/s.
	// The key [a,b] is always sorted so a ≤ b lexicographically.
	LinkCapacities map[[2]string]float64
	// HeadroomFactor is the multiplier applied to the bitrate before comparing
	// to link capacity. Values < 1 expand effective capacity; values > 1
	// shrink it. Defaults to 1.5 when zero.
	HeadroomFactor float64
	// SiblingAssets is the list of other video assets of the same work whose
	// bytes this node holds. Used to populate alternatives on R2 refusal.
	SiblingAssets []SiblingAsset
	// RendererSiteHasPeer is true when the renderer's site has an enrolled
	// peer controller. Enables the copy_to_site_first alternative.
	RendererSiteHasPeer bool
	// ControllerPeerID is this controller's own peer ID, carried into the result
	// as controlling_party.
	ControllerPeerID string
	// RenderBaseURL is this node's render-capability base URL, used as
	// byte_origin when the source is self.
	RenderBaseURL string
}

// Source mirrors the fields of routing.Candidate that Plan needs, without
// importing the routing package (which would create a cross-package dependency
// from an inner domain package).
type Source struct {
	PeerID   string
	Site     string
	IsSelf   bool
	Endpoint string
}

// SiblingAsset is a video asset of the same work that this node holds and that
// could be a smaller-copy alternative.
type SiblingAsset struct {
	AssetID     string
	HeightPx    int
	BitrateKbps float64
}

// Result is what Plan returns.
type Result struct {
	// Refused is true when the route cannot proceed.
	Refused bool
	// Route is "r1" or "r2".
	Route string
	// ControllingParty is the peer ID of this controller.
	ControllingParty string
	// ByteOrigin is the render-base URL of the byte source.
	ByteOrigin string
	// ClientMustStayConnected is always false in phase 1 (R3 is not implemented).
	ClientMustStayConnected bool
	// ReasonCode is a stable machine-readable code.
	ReasonCode string
	// ReasonHuman is a human-readable explanation.
	ReasonHuman string
	// Alternatives lists smaller-copy or copy-to-site options when the plan
	// is refused due to capacity. Empty for all other refusals.
	Alternatives []Alternative
}

// Alternative is one option surfaced when a route is refused.
type Alternative struct {
	// Kind is "smaller_copy" or "copy_to_site_first".
	Kind string
	// AssetID is set for smaller_copy.
	AssetID string
	// HeightPx is set for smaller_copy.
	HeightPx int
	// BitrateKbps is set for smaller_copy.
	BitrateKbps float64
	// Site is set for copy_to_site_first.
	Site string
}

// linkKey returns the canonical (sorted) key for the link between two sites.
func linkKey(a, b string) [2]string {
	if a <= b {
		return [2]string{a, b}
	}
	return [2]string{b, a}
}

// headroom returns the effective headroom factor, defaulting to 1.5.
func headroom(h float64) float64 {
	if h <= 0 {
		return 1.5
	}
	return h
}

// Plan decides the route for a renderer playback.
//
// Rules are applied in order:
//  1. r3_only / unreachable renderer → refuse route-r3-not-available
//  2. no source → refuse route-no-source
//  3. source site == renderer site → R1, reason route-r1-local
//  4. otherwise R2: capacity check (undeclared / unknown bitrate / within / over)
func Plan(in Input) Result {
	// Rule 1: renderer unreachable (r3_only).
	if !in.RendererReachable {
		return Result{
			Refused:     true,
			ReasonCode:  "route-r3-not-available",
			ReasonHuman: "this renderer requires relay (R3) which is not implemented yet",
		}
	}

	// Rule 2: no byte source found.
	if !in.SourceFound {
		return Result{
			Refused:     true,
			ReasonCode:  "route-no-source",
			ReasonHuman: "no peer holds a replica of this asset",
		}
	}

	byteOrigin := in.RenderBaseURL
	if !in.Source.IsSelf && in.Source.Endpoint != "" {
		byteOrigin = in.Source.Endpoint
	}

	// Rule 3: source and renderer share a site → R1 (no capacity check).
	if in.Source.Site == in.RendererSite && in.Source.Site != "" {
		return Result{
			Route:            "r1",
			ControllingParty: in.ControllerPeerID,
			ByteOrigin:       byteOrigin,
			ReasonCode:       "route-r1-local",
			ReasonHuman:      "source and renderer are on the same site",
		}
	}

	// Rule 4: cross-site → R2 with capacity check.
	key := linkKey(in.Source.Site, in.RendererSite)
	cap, declared := in.LinkCapacities[key]

	if !declared {
		return Result{
			Route:            "r2",
			ControllingParty: in.ControllerPeerID,
			ByteOrigin:       byteOrigin,
			ReasonCode:       "route-r2-capacity-undeclared",
			ReasonHuman:      "cross-site link capacity is not declared; proceeding without capacity check",
		}
	}

	if in.AssetBitrateKbps == 0 {
		return Result{
			Route:            "r2",
			ControllingParty: in.ControllerPeerID,
			ByteOrigin:       byteOrigin,
			ReasonCode:       "route-r2-bitrate-unknown",
			ReasonHuman:      "asset bitrate is unknown; proceeding without capacity check",
		}
	}

	h := headroom(in.HeadroomFactor)
	// cap is in Mbit/s; bitrate is in kbit/s. Convert bitrate to Mbit/s.
	bitrateWithHeadroom := (in.AssetBitrateKbps / 1000.0) * h
	if bitrateWithHeadroom <= cap {
		return Result{
			Route:            "r2",
			ControllingParty: in.ControllerPeerID,
			ByteOrigin:       byteOrigin,
			ReasonCode:       "route-r2-within-capacity",
			ReasonHuman:      "asset fits within the cross-site link capacity",
		}
	}

	// Over capacity: build alternatives.
	alts := buildAlternatives(in, cap, h)
	return Result{
		Refused:      true,
		ReasonCode:   "route-r2-over-capacity",
		ReasonHuman:  "asset bitrate exceeds cross-site link capacity",
		Alternatives: alts,
	}
}

// buildAlternatives constructs the list of alternatives when an R2 route is
// refused due to capacity. It lists smaller sibling copies that fit within the
// link, sorted by height descending (best fitting quality first), and
// optionally a copy_to_site_first option when the renderer's site has an
// enrolled peer.
func buildAlternatives(in Input, capMbps, h float64) []Alternative {
	var alts []Alternative

	// Smaller sibling copies that fit within the capacity.
	var fittingSiblings []SiblingAsset
	for _, s := range in.SiblingAssets {
		if s.BitrateKbps == 0 {
			continue
		}
		needed := (s.BitrateKbps / 1000.0) * h
		if needed <= capMbps {
			fittingSiblings = append(fittingSiblings, s)
		}
	}
	// Sort by height descending (best quality first).
	sort.Slice(fittingSiblings, func(i, j int) bool {
		return fittingSiblings[i].HeightPx > fittingSiblings[j].HeightPx
	})
	for _, s := range fittingSiblings {
		alts = append(alts, Alternative{
			Kind:        "smaller_copy",
			AssetID:     s.AssetID,
			HeightPx:    s.HeightPx,
			BitrateKbps: s.BitrateKbps,
		})
	}

	// Copy-to-site option when renderer's site has an enrolled peer.
	if in.RendererSiteHasPeer && in.RendererSite != "" {
		alts = append(alts, Alternative{
			Kind: "copy_to_site_first",
			Site: in.RendererSite,
		})
	}

	return alts
}
