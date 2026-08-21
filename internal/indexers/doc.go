// Package indexers implements Torznab, the discovery protocol (§59, ADR-0028).
//
// # It binds to a protocol, not to a product
//
// Torznab is `t=caps`, `t=search`, an `apikey` parameter and a fixed XML
// schema. Prowlarr serves it, Jackett serves it, and some trackers serve it
// natively. Nothing in this package knows which of those it is talking to,
// and the corpus behind it holds captures from two different servers for
// exactly that reason: one server's habits are not the protocol, and a client
// tested against one implementation is shaped to that implementation whether
// or not anybody intended it.
//
// That is not a hypothetical. The two servers measured during this work
// disagree about the most important response in the protocol — see
// [errorFromBody] — and the capture tooling's own redactor had been shaped to
// one of them closely enough to let a live credential through.
//
// # Fixtures are the only test that will ever run
//
// ADR-0026: a real indexer proxies real trackers with real credentials. It is
// not reproducible, it is not ours, and it can never run in CI. So the
// recorded corpus does not approximate reality for this package — it replaces
// it, permanently. A branch here with no fixture behind it is a branch that
// has never seen a real response.
//
// # Values in, values out
//
// The registry's Indexer contract takes a query value and returns
// []acquisition.ReleaseCandidate. No *http.Client, no base URL, no transport
// of any kind crosses that line — because a caller that could tell it was
// talking to HTTP is a caller a fixture could not stand in for, and the
// fixture strategy is the whole test strategy.
package indexers
