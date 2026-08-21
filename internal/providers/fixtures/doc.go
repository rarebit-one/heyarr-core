// Package fixtures replays recorded provider responses against the real client
// code (ADR-0026). Milestone 3.
//
// # Why a corpus exists at all
//
// A real indexer proxies real trackers with real credentials. It is not
// reproducible, it is not ours, and it can never run in CI — not as a
// convenience judgement but as a property of what it is. ADR-0026 therefore
// makes recorded fixtures the PRIMARY test strategy for indexers rather than a
// stand-in for one, and the only live exercise is out of band.
//
// That raises the stakes on the corpus considerably. For a media file, a
// hand-written fixture is merely unrealistic. For an indexer, a hand-written
// fixture is the only thing the test will ever see, so it does not approximate
// reality — it replaces it. A corpus that was invented rather than captured
// tests that the client agrees with whoever invented it.
//
// # So a capture records where it came from, and the code refuses one that does not
//
// Every fixture carries provenance: which service, which version, when, and
// the exact procedure. A committed capture with no record of what produced it
// is one nobody can regenerate and therefore nobody can trust the day it
// starts failing — the same reasoning that puts a version, a digest and a URL
// in scripts/toolchain.lock rather than just a URL.
//
// Load refuses a fixture whose provenance is missing or whose Origin is
// unrecorded. It is not decoration to be filled in later: an unlabelled
// fixture becomes a trusted one within a week.
//
// # Redaction happens at capture time, and is checked again at CI time
//
// Real captures contain real API keys, real tracker URLs and real announce
// URLs. This is a public AGPL repository and git history is permanent, so a
// leaked key is not a tidiness problem — it is a credential in a permanent
// public record.
//
// The scanner therefore runs over the committed corpus in CI rather than only
// at capture. The point is to catch the NEXT person, who will capture on a
// machine whose redaction rules are whatever they were that afternoon.
package fixtures
