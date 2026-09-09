package providers

import (
	"fmt"
	"sort"
	"strings"
)

// Capability is what an external service can do for us (§59).
//
// # The vocabulary is structured, dotted and lower-case
//
// A bare segment is the general capability; dots narrow it. `indexer` means
// "can search for releases"; a future `indexer.usenet` would mean a particular
// kind. Nothing narrows anything yet and that is fine — the shape is chosen
// now so that the first narrowing is an addition rather than a rename.
//
// The same spelling is used for worker capabilities (ffmpeg, and in a later
// milestone ffmpeg.encoder.hevc). That is deliberate: one reader, one
// vocabulary. See the package doc for why they are nonetheless separate
// mechanisms.
//
// # The set is open, and that is what stops this becoming indexer-shaped
//
// Milestone 3 ships one real implementation kind. If the type only admitted
// `indexer` and `download`, the first metadata provider would be a change to
// the registry rather than a change to configuration — and §59's whole point
// is that content types and provider kinds do not each grow their own
// registry. So `metadata` exists here with nothing implementing it, and a test
// registers one to prove routing and reporting do not care.
type Capability string

const (
	// CapabilityIndexer searches for releases. Prowlarr is the first (§59).
	CapabilityIndexer Capability = "indexer"
	// CapabilityDownload performs the transfer. Transmission is the first
	// (§58). Heyarr decides what to acquire; an external client moves bytes.
	CapabilityDownload Capability = "download"
	// CapabilityMetadata resolves what content IS, replacing Milestone 1's
	// path parser.
	//
	// Nothing implements it in Milestone 3 — §84's list does not include
	// metadata providers, and doing it properly means a third un-reproducible
	// external service plus a re-resolution sweep across the catalogue. It is
	// declared because a registry that only knew about the two kinds it ships
	// would have to be reopened for the third, which is the shape §59 exists
	// to prevent.
	CapabilityMetadata Capability = "metadata"
	// CapabilitySubtitle fetches a subtitle for content the library already
	// holds but has no caption for — neither a shipped sidecar nor an embedded
	// track (ADR-0085). OpenSubtitles is the first (M12 Phase 5).
	//
	// It is its OWN capability rather than a facet of metadata, because a
	// subtitle provider answers a different question — "which subtitle files
	// exist for this exact release" — and a metadata provider (TVDB, TMDB)
	// answers none of it. The open-set argument capability.go was built on
	// (§59) is what lets it land as one more const plus one more routing
	// accessor, not a registry reopening.
	CapabilitySubtitle Capability = "subtitle"
	// CapabilityEnrich fills in what a Work already in the library IS — its
	// canonical ids and its cover — keyed on the Work itself (ADR-0087, M12
	// Phase 6). MusicBrainz (music) and Open Library (books) are the first
	// (M12 Phase 6).
	//
	// It is its OWN capability rather than a facet of metadata for the reason
	// subtitle is: a metadata provider (TVDB, TMDB) is routed by
	// ServesType(followed.Type) for the poll loop, and enrichment routes by a
	// held Work's CONTENT type — two routing modes on one capability is the
	// overload ADR-0085 refused. An enrich provider answers a question no other
	// capability does: "given a Work — its content type and the identity
	// attributes ingest parsed — what is its canonical id, and where is its
	// cover." The open-set argument (§59) is what lets it land as one more const
	// plus one more routing accessor, not a registry reopening.
	CapabilityEnrich Capability = "enrich"
)

// Capabilities lists every capability Heyarr knows, in a stable order.
//
// Stable because it appears in error messages and in API responses, and an
// order that depends on map iteration is one nobody can diff.
func Capabilities() []Capability {
	return []Capability{CapabilityIndexer, CapabilityDownload, CapabilityMetadata, CapabilitySubtitle, CapabilityEnrich}
}

// ParseCapability validates a capability from configuration or the wire.
//
// An unknown capability is refused rather than ignored. Silently dropping one
// would let a typo — `indexr` — produce a provider that is configured, healthy
// and never routed to, which presents as "searches return nothing" and is
// nobody's first guess.
func ParseCapability(s string) (Capability, error) {
	normalised := Capability(strings.ToLower(strings.TrimSpace(s)))
	for _, c := range Capabilities() {
		if c == normalised {
			return c, nil
		}
	}
	return "", fmt.Errorf("%q is not a capability — it must be one of %s",
		s, JoinCapabilities(Capabilities()))
}

// JobCapability is the worker capability a node advertises when it holds a
// provider with this capability.
//
// This is the ONE deliberate crossing between the two vocabularies described
// in the package doc, and it is a named method rather than a bare string
// conversion so that the crossing is greppable. The strings match today; if
// they ever diverge, they diverge here and in exactly one place.
//
// The meaning of the crossing: a search job requires a node that HAS an
// indexer configured, because a node with none cannot run it. That is the same
// shape as ADR-0023's ffprobe — the job stays pending and visible rather than
// failing — and it is capability routing's second and third user after the
// media toolchain.
func (c Capability) JobCapability() string { return string(c) }

// JoinCapabilities renders a set for a message, in the canonical order.
func JoinCapabilities(caps []Capability) string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, string(c))
	}
	return strings.Join(out, ", ")
}

// sortCapabilities orders a set canonically, so that what a provider advertises
// renders identically every time it is read.
func sortCapabilities(caps []Capability) []Capability {
	rank := map[Capability]int{}
	for i, c := range Capabilities() {
		rank[c] = i
	}
	out := append([]Capability(nil), caps...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, oki := rank[out[i]]
		rj, okj := rank[out[j]]
		if oki != okj {
			// An unknown capability sorts last rather than panicking. Nothing
			// should reach here — ParseCapability refuses them — and sorting
			// is not the place to discover it.
			return oki
		}
		if ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}
