// Package followed models a FollowedSource — "archive everything this source
// emits, under this policy" (M12, the Followed Sources archive).
//
// # The whole feature is one sentence
//
// A followed source's feed adapter enumerates the items a source emits, projects
// each new item as a per-item DesiredItem at desired.ScopeItem, and lets the
// EXISTING acquisition pipeline archive it. Following = enumerate + project
// wants + get out of the way. Nothing here is a parallel pipeline: from the
// moment an item is projected it is an ordinary want walking §64's state
// machine, and every archived item lands as a managed Blob that M4/M5
// replication carries to both Full Peers for free.
//
// # Three roles, and this package owns the middle one
//
//   - The feed adapter — "what items does this source have now?" — is a
//     CapabilityMetadata provider (internal/providers). It is external and
//     un-reproducible, so it is exercised against fixtures, never in CI.
//   - The acquisition adapter — "fetch this one item" — is the existing
//     providers.Downloader. A followed source adds no new transport.
//   - The control state — the subscription itself, its policy, its poll
//     cadence, and the pure projection from an enumerated item to a want — is
//     here. It is single-writer control-plane state, modelled on desired.Item.
//
// # Source-agnostic by construction
//
// A caller expresses CONTENT INTENT — "follow this series" — and the system
// infers the Type and routes to the adapter. Type is inferred and STORED; it is
// never a knob a caller turns to pick an adapter, exactly as the provider layer
// keeps "which provider" out of the caller's hands. A projected want is an
// ordinary DesiredItem, and the search/evaluate/grab pipeline never learns which
// kind of source emitted it.
//
// # Follow is not a one-off
//
// want_content is "get this once". A FollowedSource is a standing subscription
// that keeps projecting new items as the source emits them. The two are
// deliberately different verbs over the same downstream machinery.
//
// # This package touches nothing
//
// No os, no path/filepath, no database/sql, no persistence, no CAS — depguard
// enforces it (§18, ADR-0006/0007). The row that persists a Source, the
// followbeat controller that polls due sources, and the poll_source worker that
// runs a feed round-trip live at the edge; this package is the values and the
// pure decisions they turn on.
package followed
