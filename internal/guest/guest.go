// Package guest is heyarr's anonymous, read-only browse mode (ADR-0074).
//
// A Guest is a caller with no credential, admitted deliberately when the
// operator has enabled the mode, to browse and play the shared library and
// nothing more. It is a first-class stance — an identity, a scope, and a
// content predicate — rather than the shape of a missing device cert.
//
// What lives here is the model, kept free of the HTTP layer so it can be
// reasoned about and tested on its own: who a Guest is (Identity), and what a
// Guest may see (Visible / VisibleClasses). The admission decision and the
// route enforcement live in the HTTP layer that imports this package.
package guest

import (
	"github.com/rarebit-one/voidbind-go/grant"

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// The HEYARR-side capabilities a guest access lease carries (ADR-0094). They are
// defined over grant.Capability — the bare string type voidbind-go re-exports —
// but they are OURS: the dependency knows only `read`/`write`, and coupling a
// browse/play/subtitle vocabulary into it was rejected. A guest lease grants
// exactly these three and nothing else.
const (
	// CapBrowse is reading the shared library: works, editions, assets, search,
	// discovery — the read floor the router already requires.
	CapBrowse grant.Capability = "browse"
	// CapPlay is resolving and streaming content: planning a playback, fetching a
	// stream, and — the one write-scoped route a guest's play capability opens —
	// starting a playback session (POST /playback).
	CapPlay grant.Capability = "play"
	// CapSubtitle is fetching subtitles for playable content. Fetching is a read,
	// already under the browse floor; WANTING or backfilling subtitles is a write
	// and stays closed to a guest.
	CapSubtitle grant.Capability = "subtitle"
)

// Principal is the lease principal a guest acts as (ADR-0094) — the lease store's
// first non-device principal. Free-form by the store's contract; "guest" is the
// value grant.Verify matches on exactly.
const Principal = "guest"

// ReasonCapabilityDenied is the stable, machine-readable code a guest capability
// refusal reports (ADR-0094), reused verbatim from the grant layer so a client
// branches on the same value whether the check was a lease verification or the
// HTTP capability gate. It is a code, not prose — the §63 Reason.Rule convention.
const ReasonCapabilityDenied = string(grant.ReasonCapabilityDenied)

// Capabilities is the capability set every guest lease is minted with, in a
// stable order. It is the single source of truth for what a guest may do, the
// capability analogue of visibleClasses.
func Capabilities() []grant.Capability {
	return []grant.Capability{CapBrowse, CapPlay, CapSubtitle}
}

// capabilityStrings renders capabilities for auth.Identity, whose Capabilities
// field is bare strings so the auth package need not import grant.
func capabilityStrings(caps []grant.Capability) []string {
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = string(c)
	}
	return out
}

// Source classes an asset can carry (ADR-0020). Only managed is ever written
// today; linked and vault are declared but unimplemented. They are named here
// because the guest content boundary is defined in terms of them.
const (
	// ClassManaged is content whose bytes heyarr owns in the CAS.
	ClassManaged = "managed"
	// ClassLinked is content referenced in place, with no blob of its own.
	ClassLinked = "linked"
	// ClassVault is encrypted, personal content — never guest-visible.
	ClassVault = "vault"
)

// visibleClasses is the single source of truth for the guest content boundary:
// the asset source classes a Guest may see. Anything not listed — vault today,
// and any class a newer binary writes that this one has not been taught to
// share — is not guest-visible. Fail closed: a Guest is shown only what is
// affirmatively shared, never what merely was not thought to be hidden.
var visibleClasses = map[string]bool{
	ClassManaged: true,
	ClassLinked:  true,
}

// Identity is who a Guest acts as: the read scope and nothing else, an
// anonymous `guest` principal, and the Guest marker that the personal-state
// route guards read to keep a Guest off a per-identity surface a scope check
// cannot distinguish (RefuseGuest).
//
// It is the sibling of the disabled-auth `anonymous` identity (which holds
// admin, because disabling auth is a loopback-only trust decision) and the
// web-login `session` identity (which acts as a pinned user at the read floor).
// A Guest is neither: it is nobody, at the read floor.
func Identity() auth.Identity {
	return identityWith(Capabilities())
}

// identityWith builds the guest identity carrying the capabilities of the lease
// it was minted from (ADR-0094). The scope stays exactly `read` — a guest never
// carries write — so scope alone still refuses every write route; the
// capabilities are the finer grain the route gate consults to let the `play`
// capability through to POST /playback and nothing else.
func identityWith(caps []grant.Capability) auth.Identity {
	return auth.Identity{
		Guest:     true,
		Principal: auth.Principal{Kind: "guest", Name: Principal},
		Token: auth.Token{
			Name:   Principal,
			Scopes: []auth.Scope{auth.ScopeRead},
		},
		Capabilities: capabilityStrings(caps),
	}
}

// Visible reports whether an asset of the given source class is part of the
// shared library a Guest may see (ADR-0074).
//
// This is the seam. Today every asset written is `managed`, so the predicate
// admits everything that exists and hides nothing — it is deliberately
// over-built for the current data. It becomes load-bearing the moment `vault`
// (encrypted/personal) content is implemented: a vault asset is never
// guest-visible, and this is the one place that rule is expressed.
func Visible(sourceClass string) bool {
	return visibleClasses[sourceClass]
}

// VisibleClasses returns the guest-visible source classes, sorted, so a caller
// building a SQL `source_class IN (…)` filter derives the same allowlist Visible
// enforces per item — one source of truth for the boundary, whether it is
// applied to a single asset or pushed into a paginated query.
func VisibleClasses() []string {
	out := make([]string, 0, len(visibleClasses))
	for c := range visibleClasses {
		out = append(out, c)
	}
	// A small, fixed set; a manual insertion sort keeps the order stable
	// without pulling in sort for two elements.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
