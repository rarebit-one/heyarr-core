// Package mcp exposes semantic actions over MCP (spec §71, ADR-0019).
//
// # Why this waited five milestones
//
// ADR-0019 reserved this package in Milestone 1 and deliberately left it
// empty. MCP's value is SEMANTIC ACTIONS OVER DESIRED STATE, not a second read
// API — and Milestone 1 had no desired state, no releases and no acquisition.
// Shipping then would have meant publishing `list_works` and `get_asset`, and
// then breaking that vocabulary once the interesting verbs existed.
//
// Tool vocabularies are harder to change than endpoints. An endpoint can be
// versioned, deprecated and redirected; a tool is a string an agent was built
// against, and there is no deprecation header an agent reads. So the rule this
// package follows is stricter than "ship what works": SHIP ONLY THE VERBS
// WHOSE UNDERLYING CAPABILITY EXISTS, and leave the rest ABSENT rather than
// stubbed.
//
// A tool that answers "not implemented" is worse than a missing tool. A missing
// tool is a vocabulary an agent can grow into; a stubbed one is a published
// promise with a hole in it, which is precisely what ADR-0019 waited to avoid.
// deferred.go records what is missing and which milestone brings it.
//
// # A tool is an intent, not an endpoint with a different envelope
//
// `want_content` is not `POST /desired` wearing a hat. It is "make this exist
// under these conditions", which means resolving what "this" is — possibly
// creating the Work — choosing a profile by the name a person would use, and
// creating desire. The HTTP handler and this package call the SAME exported
// intent on the resource API, because two implementations of one intent drift,
// and the drift is silent: one door emits the acquisition event and the other
// does not.
//
// # Results carry reasons, not verdicts
//
// The same principle as the playback planner and the release-candidate
// evaluator. An agent told "rejected" can do nothing. An agent told "rejected
// because minimum_resolution failed at 720" can raise it with a person, which
// is the entire reason an agent is worth connecting to a library at all.
// explain_release passes §63's reasons through with their stable codes intact
// rather than summarising them into prose.
//
// # §72 is a boundary, not a caveat
//
// Controller-side MCP CANNOT decrypt user artifacts. Private playlists,
// reading positions, ratings and history stay inaccessible unless an
// authorised user device surfaces them through a separate Personal MCP (§73,
// Milestone 9). Nothing here may expose personal state, and the boundary is
// asserted by ENUMERATING the registered tool surface rather than by anyone
// remembering — a test that will still be there when someone adds a convenient
// tool in Milestone 9.
//
// # Authentication is the existing one
//
// An MCP session is not a new trust domain. ADR-0011's scoped bearer tokens
// govern, and every tool declares the scope it needs, checked on every call. A
// read-scoped token that could call want_content would be a read token that
// can write — and no happy-path test would ever notice.
//
// # There is no MCP SDK here
//
// The protocol is JSON-RPC 2.0 and the surface this needs is three methods:
// initialize, tools/list and tools/call. Hand-writing that is under two
// hundred lines and adds no dependency to a repository that has deliberately
// stayed thin — the same reasoning ADR-0015 gives for a hand-written OpenAPI
// document. A dependency would also decide the transport for us, and this
// mounts on the existing authenticated router precisely so it inherits the
// middleware chain, the scope floor and the request correlation that already
// work.
package mcp
