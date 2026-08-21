// Package providers is the centralised provider registry: credentials,
// capabilities, routing, health, configuration (spec §59). Milestone 3.
//
// §61 lists duplicated integrations among the things Heyarr avoids, and §83's
// scope discipline says that if something needs a second registry, it is the
// same registry with a capability. So there is exactly one of these, and a
// content type never grows its own.
//
// # The interface is defined in VALUES, never in transport
//
// This is the single most consequential decision in the package, and every
// test downstream of it depends on getting it right.
//
// A real indexer talks to real trackers with real credentials. It can never
// run in CI — not because it is inconvenient to install, but because it is a
// proxy to services that are not ours and not reproducible. So a recorded
// fixture and an in-process fake are not conveniences to add later; they are
// the primary test strategy, and they only work if a fixture replayer, a fake
// and a real HTTP client are INDISTINGUISHABLE to every caller.
//
// Which means: a provider takes a query VALUE and returns
// []acquisition.ReleaseCandidate. The registry never hands out an *http.Client,
// a base URL, a transport, or anything else with a socket behind it.
// Credentials, retries, timeouts and rate limits live behind the interface, in
// the implementation, not in the caller. If a caller can tell it is talking to
// HTTP, the fixture strategy is dead before it starts.
//
// The proof is mechanical: nothing in this package's test suite opens a
// listening socket, and a test that needed one would be evidence the interface
// had leaked.
//
// # Two vocabularies, deliberately alike and deliberately separate
//
// There are two things in Heyarr called a "capability" and they are not the
// same thing:
//
//	PROVIDER capability     what an external SERVICE can do for us
//	                        lives on a provider, consumed by routing here
//	                        changes when an operator configures a service
//
//	WORKER capability       what a NODE can execute
//	                        lives on a worker, consumed by jobs.Claim()
//	                        changes when a binary, driver or device changes
//
// They share a SPELLING — structured, hierarchical, dotted, lower-case — so
// that a reader who has met one has learnt the other. They do not share a
// mechanism: jobs.required_capability is never used to route to a provider,
// because a provider is not a worker and overloading that column would make
// "which nodes can encode" and "which providers can search" the same query.
//
// Capability.JobCapability is the one deliberate bridge between them, and it
// exists so the crossing is a named function call rather than a string that
// happens to match.
//
// # Presence is checked at startup; reachability is checked continuously
//
// ADR-0023 made a configured-but-unresolvable BINARY a startup error. For a
// network service that asymmetry inverts, and ADR-0025 records why: a download
// client that is down at 03:00 must not stop Heyarr serving the library.
//
// So a syntactically invalid endpoint or a missing credential IS a startup
// error, naming the provider and the field — nobody asked for a provider they
// did not configure, and silently ignoring one they did is worse than not
// starting. But a provider that is merely unreachable starts, and is reported
// unhealthy. Health is a job (ADR-0025), which is why it lives in the registry
// rather than in each integration.
//
// # Credentials leave here only through Reveal
//
// Secret renders as a redaction in logs, in errors, in %v and in JSON. Getting
// the plaintext out requires calling Reveal, which is greppable. This is a
// public repository; an indexer API key in a debug log is a real leak with a
// permanent git history behind it.
package providers
